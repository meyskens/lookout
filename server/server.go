package server

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/meyskens/lookout"
	"github.com/meyskens/lookout/store"
	"github.com/meyskens/lookout/store/models"
	"github.com/meyskens/lookout/util/ctxlog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/meyskens/lookout-sdk.v0/pb"

	log "gopkg.in/src-d/go-log.v1"
	yaml "gopkg.in/yaml.v2"
)

var grpcErrorMessages = map[lookout.EventType]map[codes.Code]string{
	pb.PushEventType: map[codes.Code]string{
		codes.DeadlineExceeded: "timeout exceeded, try increasing analyzer_push in config.yml",
	},
	pb.ReviewEventType: map[codes.Code]string{
		codes.DeadlineExceeded: "timeout exceeded, try increasing analyzer_review in config.yml",
	},
}

// Config is a server configuration
type Config struct {
	Analyzers []lookout.AnalyzerConfig
}

type reqSent func(
	ctx context.Context,
	client lookout.AnalyzerClient,
	settings map[string]interface{},
) ([]*lookout.Comment, error)

// Server implements glue between providers / data-server / analyzers
type Server struct {
	poster         lookout.Poster
	fileGetter     lookout.FileGetter
	analyzers      map[string]lookout.Analyzer
	eventOp        store.EventOperator
	commentOp      store.CommentOperator
	organizationOp store.OrganizationOperator

	analyzerReviewTimeout time.Duration
	analyzerPushTimeout   time.Duration

	exitOnError bool
}

// Options defines the options for NewServer
type Options struct {
	Poster     lookout.Poster
	FileGetter lookout.FileGetter
	Analyzers  map[string]lookout.Analyzer

	// EventOp is the operator for the Event persistence. Can be left unset.
	EventOp store.EventOperator
	// CommentOp is the operator for the Comment persistence. Can be left unset.
	CommentOp store.CommentOperator
	// OrganizationOp is the operator for the Organization persistence. Can be left unset.
	OrganizationOp store.OrganizationOperator

	// ReviewTimeout is the timeout for an analyzer to reply a NotifyReviewEvent.
	// Zero means no timeout.
	ReviewTimeout time.Duration
	// PushTimeout is the timeout for an analyzer to reply a NotifyPushEvent.
	// Zero means no timeout.
	PushTimeout time.Duration

	// ExitOnError set to true will stop the server and return an error
	// if any analyzer Notify* call or a posting call fails
	ExitOnError bool
}

// NewServer creates a new Server with the given options
func NewServer(opt Options) *Server {
	server := Server{
		poster:                opt.Poster,
		fileGetter:            opt.FileGetter,
		analyzers:             opt.Analyzers,
		eventOp:               opt.EventOp,
		commentOp:             opt.CommentOp,
		organizationOp:        opt.OrganizationOp,
		analyzerReviewTimeout: opt.ReviewTimeout,
		analyzerPushTimeout:   opt.PushTimeout,
		exitOnError:           opt.ExitOnError,
	}

	if opt.EventOp == nil {
		server.eventOp = &store.NoopEventOperator{}
	}

	if opt.CommentOp == nil {
		server.commentOp = &store.NoopCommentOperator{}
	}

	if opt.OrganizationOp == nil {
		server.organizationOp = &store.NoopOrganizationOperator{}
	}

	return &server
}

// HandleEvent processes the event calling the analyzers, and posting the results
func (s *Server) HandleEvent(ctx context.Context, e lookout.Event) error {
	ctx, logger := ctxlog.WithLogFields(ctx, log.Fields{
		"event-type": reflect.TypeOf(e).String(),
		"event-id":   e.ID().String(),
		"repo":       e.Revision().Head.InternalRepositoryURL,
		"head":       e.Revision().Head.ReferenceName,
	})

	status, err := s.eventOp.Save(ctx, e)
	if err != nil {
		logger.Errorf(err, "can't save event to database")
		return err
	}

	if status == models.EventStatusProcessed {
		logger.Debugf("event successfully processed, skipping...")
		return nil
	}

	// TODO(max): we need some retry policy here depends on errors
	if status == models.EventStatusFailed {
		logger.Debugf("event processing failed, skipping...")
		return nil
	}

	// positing started before but never changed to success of failure
	// we need to retry analyzis but post only new comments (poster should handle it)
	safePosting := status == models.EventStatusPosting

	switch ev := e.(type) {
	case *lookout.ReviewEvent:
		err = s.HandleReview(ctx, ev, safePosting)
	case *lookout.PushEvent:
		err = s.HandlePush(ctx, ev, safePosting)
	default:
		logger.Debugf("ignoring unsupported event: %s", ev)
	}

	if err == nil {
		status = models.EventStatusProcessed
	} else {
		logger.Errorf(err, "event processing failed")
		status = models.EventStatusFailed
	}

	if updateErr := s.eventOp.UpdateStatus(ctx, e, status); updateErr != nil {
		logger.Errorf(updateErr, "can't update status in database")
	}

	// don't fail on event processing error, just skip it
	if !s.exitOnError {
		return nil
	}

	return err
}

// HandleReview sends request to analyzers concurrently
func (s *Server) HandleReview(ctx context.Context, e *lookout.ReviewEvent, safePosting bool) error {
	ctx, logger := ctxlog.WithLogFields(ctx, log.Fields{
		"provider": e.Provider,
	})
	logger.Infof("processing pull request")

	if err := e.Validate(); err != nil {
		return err
	}

	repoConf, err := s.getConfig(ctx, e)
	if err != nil {
		return err
	}

	orgConf, err := s.getOrgConfig(ctx, e)
	if err != nil {
		return err
	}

	conf := mergeConfigs(orgConf, repoConf)

	s.status(ctx, e, lookout.PendingAnalysisStatus)

	send := func(
		ctx context.Context,
		a lookout.AnalyzerClient,
		settings map[string]interface{},
	) ([]*lookout.Comment, error) {
		st := pb.ToStruct(settings)
		if st != nil {
			e.Configuration = *st
		}

		if s.analyzerReviewTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, s.analyzerReviewTimeout)
			defer cancel()
		}

		resp, err := a.NotifyReviewEvent(ctx, &e.ReviewEvent)
		if err != nil {
			return nil, err
		}
		return resp.Comments, nil
	}
	comments, err := s.concurrentRequest(ctx, conf, send, grpcErrorMessages[pb.ReviewEventType])
	if err != nil {
		return err
	}

	if err := s.post(ctx, e, comments, safePosting); err != nil {
		s.status(ctx, e, lookout.ErrorAnalysisStatus)
		return fmt.Errorf("posting analysis failed: %s", err)
	}

	s.status(ctx, e, lookout.SuccessAnalysisStatus)

	return nil
}

// HandlePush sends request to analyzers concurrently
func (s *Server) HandlePush(ctx context.Context, e *lookout.PushEvent, safePosting bool) error {
	ctx, logger := ctxlog.WithLogFields(ctx, log.Fields{
		"provider": e.Provider,
	})
	logger.Infof("processing push")

	if err := e.Validate(); err != nil {
		return err
	}

	repoConf, err := s.getConfig(ctx, e)
	if err != nil {
		return err
	}

	orgConf, err := s.getOrgConfig(ctx, e)
	if err != nil {
		return err
	}

	conf := mergeConfigs(orgConf, repoConf)

	s.status(ctx, e, lookout.PendingAnalysisStatus)

	send := func(
		ctx context.Context,
		a lookout.AnalyzerClient,
		settings map[string]interface{},
	) ([]*lookout.Comment, error) {
		st := pb.ToStruct(settings)
		if st != nil {
			e.Configuration = *st
		}

		if s.analyzerPushTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, s.analyzerPushTimeout)
			defer cancel()
		}

		resp, err := a.NotifyPushEvent(ctx, &e.PushEvent)
		if err != nil {
			return nil, err
		}
		return resp.Comments, nil
	}
	comments, err := s.concurrentRequest(ctx, conf, send, grpcErrorMessages[pb.PushEventType])
	if err != nil {
		return err
	}

	if err := s.post(ctx, e, comments, safePosting); err != nil {
		s.status(ctx, e, lookout.ErrorAnalysisStatus)
		return fmt.Errorf("posting analysis failed: %s", err)
	}
	s.status(ctx, e, lookout.SuccessAnalysisStatus)

	return nil
}

func (s *Server) getConfig(ctx context.Context, e lookout.Event) (map[string]lookout.AnalyzerConfig, error) {
	rev := e.Revision()
	ctxlog.Get(ctx).Debugf("getting .lookout.yml")
	scanner, err := s.fileGetter.GetFiles(ctx, &lookout.FilesRequest{
		Revision:       &rev.Head,
		IncludePattern: `^\.lookout\.yml$`,
		WantContents:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("Can't get .lookout.yml in revision %s: %s", rev.Head, err)
	}
	var configContent []byte
	if scanner.Next() {
		configContent = scanner.File().Content
	}
	scanner.Close()
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if len(configContent) == 0 {
		ctxlog.Get(ctx).Infof("repository config is not found")
		return nil, nil
	}

	parseCtx, _ := ctxlog.WithLogFields(ctx, log.Fields{"config-file": "repository .lookout.yml"})
	conf, err := s.parseConfig(parseCtx, configContent)
	if err != nil {
		return nil, fmt.Errorf("failed to get the local .lookout.yml file from the repository: %s", err)
	}

	return conf, nil
}

func (s *Server) parseConfig(ctx context.Context, configContent []byte) (map[string]lookout.AnalyzerConfig, error) {
	var conf Config
	if err := yaml.Unmarshal(configContent, &conf); err != nil {
		return nil, fmt.Errorf("can't parse configuration file: %s", err)
	}

	res := make(map[string]lookout.AnalyzerConfig, len(s.analyzers))
	for name, a := range s.analyzers {
		res[name] = a.Config
	}
	for _, aConf := range conf.Analyzers {
		if _, ok := s.analyzers[aConf.Name]; !ok {
			ctxlog.Get(ctx).Warningf("analyzer '%s' required by configuration file isn't enabled on server", aConf.Name)
			continue
		}
		res[aConf.Name] = aConf
	}

	return res, nil
}

func (s *Server) getOrgConfig(ctx context.Context, e lookout.Event) (map[string]lookout.AnalyzerConfig, error) {
	configContent, err := s.organizationOp.Config(ctx, e.GetProvider(), e.GetOrganizationID())
	if err != nil {
		return nil, fmt.Errorf("could not load default configuration for organization from the DB: %s", err)
	}

	parseCtx, _ := ctxlog.WithLogFields(ctx, log.Fields{"config-file": "organization default"})
	conf, err := s.parseConfig(parseCtx, []byte(configContent))
	if err != nil {
		return nil, fmt.Errorf("failed to get the organization default configuration from the DB: %s", err)
	}

	return conf, nil
}

func (s *Server) concurrentRequest(ctx context.Context, conf map[string]lookout.AnalyzerConfig, send reqSent, logErrorMessages map[codes.Code]string) ([]lookout.AnalyzerComments, error) {
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()

	commentsCh := make(chan *lookout.AnalyzerComments, len(s.analyzers))
	errCh := make(chan error)

	for name, a := range s.analyzers {
		if a.Config.Disabled || conf[name].Disabled {
			ctxlog.Get(ctx).Infof("analyzer %s disabled by local repository configuration", name)
			commentsCh <- nil
			continue
		}

		go func(name string, a lookout.Analyzer) {
			var result *lookout.AnalyzerComments
			defer func() { commentsCh <- result }()

			ctx, aLogger := ctxlog.WithLogFields(ctx, log.Fields{
				"analyzer": name,
			})

			settings := mergeSettings(a.Config.Settings, conf[name].Settings)

			cs, err := send(ctx, a.Client, settings)
			if err != nil {
				grpcStatus := status.Convert(err)
				errMessage := "analysis failed"
				friendlyMessage, ok := logErrorMessages[grpcStatus.Code()]
				if ok {
					errMessage = fmt.Sprintf("%s: %s", errMessage, friendlyMessage)
				}

				aLogger.Errorf(err, errMessage)

				if s.exitOnError {
					errCh <- err
				}

				return
			}

			if len(cs) == 0 {
				aLogger.Infof("no comments were produced")
				return
			}

			result = &lookout.AnalyzerComments{
				Config:   a.Config,
				Comments: cs,
			}
		}(name, a)
	}

	var comments []lookout.AnalyzerComments
	for i := 0; i < len(s.analyzers); i++ {
		select {
		case err := <-errCh:
			return nil, err
		case cs := <-commentsCh:
			if cs != nil {
				comments = append(comments, *cs)
			}
		}
	}

	return comments, nil
}

func mergeConfigs(global, local map[string]lookout.AnalyzerConfig) map[string]lookout.AnalyzerConfig {
	if local == nil {
		return global
	}

	if global == nil {
		return local
	}

	merged := make(map[string]lookout.AnalyzerConfig)
	for k, v := range global {
		merged[k] = v
	}

	for k, v := range local {
		if globalV, ok := merged[k]; ok {
			globalV.Settings = mergeMaps(globalV.Settings, v.Settings)
			merged[k] = globalV
			continue
		}

		merged[k] = v
	}

	return merged
}

func mergeSettings(global, local map[string]interface{}) map[string]interface{} {
	if local == nil {
		return global
	}

	if global == nil {
		return local
	}

	return mergeMaps(global, local)
}

func mergeMaps(global, local map[string]interface{}) map[string]interface{} {
	merged := make(map[string]interface{})
	for k, v := range global {
		merged[k] = v
	}
	for k, v := range local {
		if subMap, ok := v.(map[string]interface{}); ok {
			gv, ok := merged[k]
			if ok {
				if gvMap, ok := gv.(map[string]interface{}); ok {
					merged[k] = mergeMaps(gvMap, subMap)
					continue
				}
			}
		}
		merged[k] = v
	}

	return merged
}

func (s *Server) post(ctx context.Context, e lookout.Event, comments lookout.AnalyzerCommentsGroups, safe bool) error {
	comments, err := comments.Dedup().Filter(func(c *lookout.Comment) (bool, error) {
		yes, err := s.commentOp.Posted(ctx, e, c)
		if err != nil {
			ctxlog.Get(ctx).Errorf(err, "comment posted check failed")
			return false, err
		}

		return yes, nil
	})
	if err != nil {
		return err
	}

	if len(comments) == 0 {
		return nil
	}

	// update event status just before posting comments
	// in case the server would die while doing it we will know that process has started
	// and poster can handle it correctly
	if err := s.eventOp.UpdateStatus(ctx, e, models.EventStatusPosting); err != nil {
		return err
	}

	ctxlog.Get(ctx).With(log.Fields{
		"comments": comments.Count(),
	}).Infof("posting analysis")

	if err := s.poster.Post(ctx, e, comments, safe); err != nil {
		return err
	}

	for _, cg := range comments {
		for _, c := range cg.Comments {
			if err := s.commentOp.Save(ctx, e, c, cg.Config.Name); err != nil {
				ctxlog.Get(ctx).Errorf(err, "can't save comment")
			}
		}
	}

	return nil
}

func (s *Server) status(ctx context.Context, e lookout.Event, st lookout.AnalysisStatus) {
	if err := s.poster.Status(ctx, e, st); err != nil {
		ctxlog.Get(ctx).With(log.Fields{"status": st}).Errorf(err, "posting status failed")
	}
}

type LogPoster struct {
	Log log.Logger
}

func (p *LogPoster) Post(ctx context.Context, e lookout.Event,
	aCommentsList []lookout.AnalyzerComments, safe bool) error {
	for _, aComments := range aCommentsList {
		for _, c := range aComments.Comments {
			logger := p.Log.With(log.Fields{
				"text": c.Text,
			})
			if c.File == "" {
				logger.Infof("global comment")
				continue
			}

			logger = logger.With(log.Fields{"file": c.File})
			if c.Line == 0 {
				logger.Infof("file comment")
				continue
			}

			logger.With(log.Fields{"line": c.Line}).Infof("line comment")
		}
	}

	return nil
}

func (p *LogPoster) Status(ctx context.Context, e lookout.Event,
	status lookout.AnalysisStatus) error {
	p.Log.Infof("status: %s", status)
	return nil
}

var _ lookout.Poster = &LogPoster{}
