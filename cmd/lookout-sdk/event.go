package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gogo/protobuf/types"
	"github.com/meyskens/lookout"
	"github.com/meyskens/lookout/service/bblfsh"
	"github.com/meyskens/lookout/service/enry"
	"github.com/meyskens/lookout/service/git"
	"github.com/meyskens/lookout/service/purge"
	"github.com/meyskens/lookout/util/cli"
	"github.com/meyskens/lookout/util/grpchelper"
	"google.golang.org/grpc"

	gogit "gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	log "gopkg.in/src-d/go-log.v1"
	"gopkg.in/meyskens/lookout-sdk.v0/pb"
)

type EventCommand struct {
	cli.LogOptions
	DataServer string `long:"data-server" default:"ipv4://localhost:10301" env:"LOOKOUT_DATA_SERVER" description:"gRPC URL to bind the data server to"`
	Bblfshd    string `long:"bblfshd" default:"ipv4://localhost:9432" env:"LOOKOUT_BBLFSHD" description:"gRPC URL of the Bblfshd server"`
	GitDir     string `long:"git-dir" default:"." env:"GIT_DIR" description:"path to the .git directory to analyze"`
	RevFrom    string `long:"from" default:"HEAD^" description:"name of the base revision for event"`
	RevTo      string `long:"to" default:"HEAD" description:"name of the head revision for event"`
	ConfigJSON string `long:"config-json" description:"arbitrary JSON configuration for request to an analyzer"`
	Args       struct {
		Analyzer string `positional-arg-name:"analyzer" description:"gRPC URL of the analyzer to use (default: ipv4://localhost:9930)"`
	} `positional-args:"yes"`

	repo *gogit.Repository
}

func (c *EventCommand) openRepository() error {
	var err error

	c.repo, err = gogit.PlainOpenWithOptions(c.GitDir, &gogit.PlainOpenOptions{
		// it's useful to walk to parent in case --git-dir is default
		// but very confusing in all other cases
		DetectDotGit: c.GitDir == ".",
	})

	if err != nil {
		return fmt.Errorf("can't open repository at path '%s': %s", c.GitDir, err)
	}

	return nil
}

func (c *EventCommand) resolveRefs() (*lookout.ReferencePointer, *lookout.ReferencePointer, error) {
	log.Infof("resolving to/from references")
	baseHash, err := getCommitHashByRev(c.repo, c.RevFrom)
	if err != nil {
		return nil, nil, fmt.Errorf("base revision '%s' error: %s", c.RevFrom, err)
	}

	headHash, err := getCommitHashByRev(c.repo, c.RevTo)
	if err != nil {
		return nil, nil, fmt.Errorf("head revision '%s' error: %s", c.RevTo, err)
	}

	fullGitPath, err := filepath.Abs(c.GitDir)
	if err != nil {
		return nil, nil, fmt.Errorf("can't resolve '%s' full path: %s", c.GitDir, err)
	}

	fromRef := lookout.ReferencePointer{
		InternalRepositoryURL: "file://" + fullGitPath,
		ReferenceName:         plumbing.HEAD,
		Hash:                  baseHash,
	}

	toRef := lookout.ReferencePointer{
		InternalRepositoryURL: "file://" + fullGitPath,
		ReferenceName:         plumbing.HEAD,
		Hash:                  headHash,
	}

	return &fromRef, &toRef, nil
}

type dataService interface {
	lookout.ChangeGetter
	lookout.FileGetter
}

func (c *EventCommand) makeDataServerHandler() (*lookout.DataServerHandler, error) {
	var err error

	var dataService dataService

	loader := git.NewStorerCommitLoader(c.repo.Storer)
	dataService = git.NewService(loader)
	dataService = enry.NewService(dataService, dataService)

	grpcAddr, err := pb.ToGoGrpcAddress(c.Bblfshd)
	if err != nil {
		return nil, fmt.Errorf("Can't resolve bblfsh address '%s': %s", c.Bblfshd, err)
	}
	timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bblfshConn, err := grpchelper.DialContext(timeoutCtx, grpcAddr, grpc.WithBlock())
	if err != nil {
		log.Warningf("bblfshd instance could not be found at %s. No UASTs will be available to analyzers. Error: %s", c.Bblfshd, err)
		dataService = &noBblfshService{
			changes: dataService,
			files:   dataService,
		}
	} else {
		dataService = bblfsh.NewService(dataService, dataService, bblfshConn, 0)
	}

	dataService = purge.NewService(dataService, dataService)

	srv := &lookout.DataServerHandler{
		ChangeGetter: dataService,
		FileGetter:   dataService,
	}

	return srv, nil
}

type startFunc func() error
type stopFunc func()

func (c *EventCommand) initDataServer(srv *lookout.DataServerHandler) (startFunc, stopFunc) {
	var grpcSrv *grpc.Server

	start := func() error {
		log.Infof("starting a DataServer at %s", c.DataServer)
		bblfshGrpcAddr, err := pb.ToGoGrpcAddress(c.Bblfshd)
		if err != nil {
			return fmt.Errorf("Can't resolve bblfsh address '%s': %s", c.Bblfshd, err)
		}

		grpcSrv, err = grpchelper.NewBblfshProxyServer(bblfshGrpcAddr)
		if err != nil {
			return fmt.Errorf("Can't start bblfsh proxy server: %s", err)
		}

		lookout.RegisterDataServer(grpcSrv, srv)

		lis, err := pb.Listen(c.DataServer)
		if err != nil {
			return fmt.Errorf("Can't start data server at '%s': %s", c.DataServer, err)
		}

		return grpcSrv.Serve(lis)
	}

	stop := func() {
		if grpcSrv == nil {
			return
		}

		grpcSrv.GracefulStop()
	}

	return start, stop
}

func (c *EventCommand) analyzer() (lookout.Analyzer, error) {
	if c.Args.Analyzer == "" {
		c.Args.Analyzer = "ipv4://localhost:9930"
	}

	var err error
	log.Infof("starting looking for Analyzer at %s", c.Args.Analyzer)

	grpcAddr, err := pb.ToGoGrpcAddress(c.Args.Analyzer)
	if err != nil {
		return lookout.Analyzer{}, fmt.Errorf("Can't resolve address of analyzer '%s': %s", c.Args.Analyzer, err)
	}

	timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := grpchelper.DialContext(
		timeoutCtx,
		grpcAddr,
		grpc.WithBlock(),
	)
	if err != nil {
		return lookout.Analyzer{}, fmt.Errorf("Can't connect to analyzer '%s': %s", grpcAddr, err)
	}

	client := lookout.NewAnalyzerClient(conn)

	return lookout.Analyzer{
		Client: client,
		Config: lookout.AnalyzerConfig{
			Name: "test-analyzer",
			Addr: c.Args.Analyzer,
		},
	}, nil
}

func (c *EventCommand) parseConfig() (types.Struct, error) {
	if c.ConfigJSON == "" {
		return types.Struct{}, nil
	}

	var conf map[string]interface{}
	if err := json.Unmarshal([]byte(c.ConfigJSON), &conf); err != nil {
		return types.Struct{}, fmt.Errorf("Can't parse config-json option: %s", err)
	}

	st := pb.ToStruct(conf)
	if st == nil {
		return types.Struct{}, nil
	}

	return *st, nil
}

func getCommitHashByRev(r *gogit.Repository, revName string) (string, error) {
	if revName == "" {
		return "", errors.New("Revision can't be empty")
	}

	h, err := r.ResolveRevision(plumbing.Revision(revName))
	if err != nil {
		return "", err
	}

	return h.String(), nil
}

type noBblfshService struct {
	changes lookout.ChangeGetter
	files   lookout.FileGetter
}

var _ lookout.ChangeGetter = &noBblfshService{}
var _ lookout.FileGetter = &noBblfshService{}

var errNoBblfsh = errors.New("Data server was started without bbflsh. WantUAST isn't allowed")

func (s *noBblfshService) GetChanges(ctx context.Context, req *lookout.ChangesRequest) (lookout.ChangeScanner, error) {
	if req.WantUAST {
		return nil, errNoBblfsh
	}

	return s.changes.GetChanges(ctx, req)
}

func (s *noBblfshService) GetFiles(ctx context.Context, req *lookout.FilesRequest) (lookout.FileScanner, error) {
	if req.WantUAST {
		return nil, errNoBblfsh
	}

	return s.files.GetFiles(ctx, req)
}
