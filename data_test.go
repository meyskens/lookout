package lookout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"gopkg.in/meyskens/lookout-sdk.v0/pb"
)

func setupDataServer(t *testing.T, dr *MockService) (*grpc.Server,
	pb.DataClient) {

	t.Helper()
	require := require.New(t)

	srv := &DataServerHandler{ChangeGetter: dr, FileGetter: dr}
	grpcServer := grpc.NewServer()
	pb.RegisterDataServer(grpcServer, srv)

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(err)
	address := lis.Addr().String()

	go grpcServer.Serve(lis)

	conn, err := grpc.Dial(address, grpc.WithInsecure())
	require.NoError(err)

	client := pb.NewDataClient(conn)

	return grpcServer, client
}

func tearDownDataServer(t *testing.T, srv *grpc.Server) {
	if srv != nil {
		srv.Stop()
	}
}

func TestServerGetChangesOk(t *testing.T) {
	for i := 0; i <= 10; i++ {
		req := &ChangesRequest{
			Head: &ReferencePointer{
				InternalRepositoryURL: "repo",
				Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
			},
		}
		changes := generateChanges(i)
		dr := &MockService{
			T:                t,
			ExpectedCRequest: req,
			ChangeScanner:    &SliceChangeScanner{Changes: changes},
		}
		srv, client := setupDataServer(t, dr)

		t.Run(fmt.Sprintf("size-%d", i), func(t *testing.T) {
			require := require.New(t)

			respClient, err := client.GetChanges(context.TODO(), req)
			require.NoError(err)
			require.NotNil(respClient)
			require.NoError(respClient.CloseSend())

			for _, change := range changes {
				actualResp, err := respClient.Recv()
				require.NoError(err)
				require.Equal(change, actualResp)
			}

			actualResp, err := respClient.Recv()
			require.Equal(io.EOF, err)
			require.Zero(actualResp)
		})

		tearDownDataServer(t, srv)
	}
}

func TestServerGetFilesOk(t *testing.T) {
	for i := 0; i <= 10; i++ {
		req := &FilesRequest{
			Revision: &ReferencePointer{
				InternalRepositoryURL: "repo",
				Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
			},
		}
		files := generateFiles(i)
		dr := &MockService{
			T:                t,
			ExpectedFRequest: req,
			FileScanner:      &SliceFileScanner{Files: files},
		}
		srv, client := setupDataServer(t, dr)

		t.Run(fmt.Sprintf("size-%d", i), func(t *testing.T) {
			require := require.New(t)

			respClient, err := client.GetFiles(context.TODO(), req)
			require.NoError(err)
			require.NotNil(respClient)
			require.NoError(respClient.CloseSend())

			for _, change := range files {
				actualResp, err := respClient.Recv()
				require.NoError(err)
				require.Equal(change, actualResp)
			}

			actualResp, err := respClient.Recv()
			require.Equal(io.EOF, err)
			require.Zero(actualResp)
		})

		tearDownDataServer(t, srv)
	}
}

func TestDataServerHandlerCancel(t *testing.T) {
	require := require.New(t)

	revision := &ReferencePointer{
		InternalRepositoryURL: "repo",
		Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
	}
	changesReq := &ChangesRequest{Head: revision}
	filesReq := &FilesRequest{Revision: revision}
	changes := generateChanges(1)
	files := generateFiles(1)
	changeTick := make(chan struct{}, 1)
	fileTick := make(chan struct{}, 1)
	dr := &MockService{
		T:                t,
		ExpectedCRequest: changesReq,
		ExpectedFRequest: filesReq,
		ChangeScanner: &SliceChangeScanner{
			Changes:    changes,
			ChangeTick: changeTick,
		},
		FileScanner: &SliceFileScanner{
			Files:    files,
			FileTick: fileTick,
		},
	}
	h := &DataServerHandler{ChangeGetter: dr, FileGetter: dr}

	// create cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// MockService doesn't respect context cancellation
	// which means only DataServerHandler would handle it
	changesSrv := &mockDataGetChangesServer{
		mockServerStream: &mockServerStream{ctx}}
	err := h.GetChanges(changesReq, changesSrv)
	require.EqualError(err, "request canceled: context canceled")

	filesSrv := &mockDataGetFilesServer{
		mockServerStream: &mockServerStream{ctx}}
	err = h.GetFiles(filesReq, filesSrv)
	require.EqualError(err, "request canceled: context canceled")
}

func TestDataServerHandlerSendError(t *testing.T) {
	require := require.New(t)

	revision := &ReferencePointer{
		InternalRepositoryURL: "repo",
		Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
	}
	changesReq := &ChangesRequest{Head: revision}
	filesReq := &FilesRequest{Revision: revision}
	changes := generateChanges(1)
	files := generateFiles(1)
	changeTick := make(chan struct{}, 1)
	fileTick := make(chan struct{}, 1)
	dr := &MockService{
		T:                t,
		ExpectedCRequest: changesReq,
		ExpectedFRequest: filesReq,
		ChangeScanner: &SliceChangeScanner{
			Changes:    changes,
			ChangeTick: changeTick,
		},
		FileScanner: &SliceFileScanner{
			Files:    files,
			FileTick: fileTick,
		},
	}
	h := &DataServerHandler{ChangeGetter: dr, FileGetter: dr}

	changesSrv := &mockDataGetChangesServer{
		sendReturnErr:    true,
		mockServerStream: &mockServerStream{context.Background()},
	}
	changeTick <- struct{}{}
	err := h.GetChanges(changesReq, changesSrv)
	require.EqualError(err, "send error")

	filesSrv := &mockDataGetFilesServer{
		sendReturnErr:    true,
		mockServerStream: &mockServerStream{context.Background()}}
	fileTick <- struct{}{}
	err = h.GetFiles(filesReq, filesSrv)
	require.EqualError(err, "send error")
}

type mockDataGetChangesServer struct {
	sendReturnErr bool
	*mockServerStream
}

// Send implements pb.Data_GetChangesServer
func (s *mockDataGetChangesServer) Send(*pb.Change) error {
	if s.sendReturnErr {
		return fmt.Errorf("send error")
	}

	return nil
}

type mockDataGetFilesServer struct {
	sendReturnErr bool
	*mockServerStream
}

// Send implements pb.Data_GetFilesServer
func (s *mockDataGetFilesServer) Send(*pb.File) error {
	if s.sendReturnErr {
		return fmt.Errorf("send error")
	}

	return nil
}

// Implements only `Context() context.Context` method to be able to test client cancellation
type mockServerStream struct {
	ctx context.Context
}

func (s *mockServerStream) SetHeader(metadata.MD) error {
	return nil
}
func (s *mockServerStream) SendHeader(metadata.MD) error {
	return nil
}
func (s *mockServerStream) SetTrailer(metadata.MD) {}
func (s *mockServerStream) Context() context.Context {
	return s.ctx
}
func (s *mockServerStream) SendMsg(m interface{}) error {
	return nil
}
func (s *mockServerStream) RecvMsg(m interface{}) error {
	return nil
}

func TestServerGetChangesError(t *testing.T) {
	req := &ChangesRequest{
		Head: &ReferencePointer{
			InternalRepositoryURL: "repo",
			Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
		},
	}
	changes := generateChanges(10)
	ExpectedError := fmt.Errorf("TEST ERROR")
	dr := &MockService{
		T:                t,
		ExpectedCRequest: req,
		Error:            ExpectedError,
		ChangeScanner: &SliceChangeScanner{
			Changes: changes,
		},
	}
	srv, client := setupDataServer(t, dr)

	t.Run("test", func(t *testing.T) {
		require := require.New(t)
		respClient, err := client.GetChanges(context.TODO(), req)
		require.NoError(err)
		require.NotNil(respClient)

		change, err := respClient.Recv()
		require.Error(err)
		require.Contains(err.Error(), ExpectedError.Error())
		require.Zero(change)
	})

	tearDownDataServer(t, srv)
}

func TestServerGetFilesError(t *testing.T) {
	req := &FilesRequest{
		Revision: &ReferencePointer{
			InternalRepositoryURL: "repo",
			Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
		},
	}
	files := generateFiles(10)
	ExpectedError := fmt.Errorf("TEST ERROR")
	dr := &MockService{
		T:                t,
		ExpectedFRequest: req,
		Error:            ExpectedError,
		FileScanner:      &SliceFileScanner{Files: files},
	}
	srv, client := setupDataServer(t, dr)

	t.Run("test", func(t *testing.T) {
		require := require.New(t)
		respClient, err := client.GetFiles(context.TODO(), req)
		require.NoError(err)
		require.NotNil(respClient)

		change, err := respClient.Recv()
		require.Error(err)
		require.Contains(err.Error(), ExpectedError.Error())
		require.Zero(change)
	})

	tearDownDataServer(t, srv)
}

func TestServerGetChangesIterError(t *testing.T) {
	req := &ChangesRequest{
		Head: &ReferencePointer{
			InternalRepositoryURL: "repo",
			Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
		},
	}
	changes := generateChanges(10)
	ExpectedError := fmt.Errorf("TEST ERROR")
	dr := &MockService{
		T:                t,
		ExpectedCRequest: req,
		ChangeScanner: &SliceChangeScanner{
			Changes: changes,
			Error:   ExpectedError,
		},
	}
	srv, client := setupDataServer(t, dr)

	t.Run("test", func(t *testing.T) {
		require := require.New(t)
		respClient, err := client.GetChanges(context.TODO(), req)
		require.NoError(err)
		require.NotNil(respClient)

		change, err := respClient.Recv()
		require.Error(err)
		require.Contains(err.Error(), ExpectedError.Error())
		require.Zero(change)
	})

	tearDownDataServer(t, srv)
}

func TestServerGetFilesIterError(t *testing.T) {
	req := &FilesRequest{
		Revision: &ReferencePointer{
			InternalRepositoryURL: "repo",
			Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
		},
	}
	files := generateFiles(10)
	ExpectedError := fmt.Errorf("TEST ERROR")
	dr := &MockService{
		T:                t,
		ExpectedFRequest: req,
		FileScanner: &SliceFileScanner{
			Files: files,
			Error: ExpectedError,
		},
	}
	srv, client := setupDataServer(t, dr)

	t.Run("test", func(t *testing.T) {
		require := require.New(t)
		respClient, err := client.GetFiles(context.TODO(), req)
		require.NoError(err)
		require.NotNil(respClient)

		change, err := respClient.Recv()
		require.Error(err)
		require.Contains(err.Error(), ExpectedError.Error())
		require.Zero(change)
	})

	tearDownDataServer(t, srv)
}

func generateChanges(size int) []*Change {
	var changes []*Change
	for i := 0; i < size; i++ {
		changes = append(changes, &Change{
			Head: &File{
				Path: fmt.Sprintf("myfile%d", i),
			},
		})
	}

	return changes
}

func generateFiles(size int) []*File {
	var files []*File
	for i := 0; i < size; i++ {
		files = append(files, &File{
			Path: fmt.Sprintf("myfile%d", i),
		})
	}

	return files
}

type MockService struct {
	T                *testing.T
	ExpectedCRequest *ChangesRequest
	ExpectedFRequest *FilesRequest
	ChangeScanner    ChangeScanner
	FileScanner      FileScanner
	Error            error
}

func (r *MockService) GetChanges(ctx context.Context, req *ChangesRequest) (
	ChangeScanner, error) {
	require := require.New(r.T)
	require.Equal(r.ExpectedCRequest, req)
	return r.ChangeScanner, r.Error
}

func (r *MockService) GetFiles(ctx context.Context, req *FilesRequest) (
	FileScanner, error) {
	require := require.New(r.T)
	require.Equal(r.ExpectedFRequest, req)
	return r.FileScanner, r.Error
}

type SliceChangeScanner struct {
	Changes    []*Change
	Error      error
	ChangeTick chan struct{}
	val        *Change
}

func (s *SliceChangeScanner) Next() bool {
	if s.Error != nil {
		return false
	}

	if len(s.Changes) == 0 {
		s.val = nil
		return false
	}

	s.val, s.Changes = s.Changes[0], s.Changes[1:]
	return true
}

func (s *SliceChangeScanner) Err() error {
	return s.Error
}

func (s *SliceChangeScanner) Change() *Change {
	if s.ChangeTick != nil {
		<-s.ChangeTick
	}

	return s.val
}

func (s *SliceChangeScanner) Close() error {
	return nil
}

type SliceFileScanner struct {
	Files    []*File
	Error    error
	FileTick chan struct{}
	val      *File
}

func (s *SliceFileScanner) Next() bool {
	if s.Error != nil {
		return false
	}

	if len(s.Files) == 0 {
		s.val = nil
		return false
	}

	s.val, s.Files = s.Files[0], s.Files[1:]
	return true
}

func (s *SliceFileScanner) Err() error {
	return s.Error
}

func (s *SliceFileScanner) File() *File {
	if s.FileTick != nil {
		<-s.FileTick
	}

	return s.val
}

func (s *SliceFileScanner) Close() error {
	return nil
}

func TestFnFileScanner(t *testing.T) {
	require := require.New(t)

	files := generateFiles(3)

	sliceScanner := &SliceFileScanner{Files: files}

	fn := func(f *File) (bool, error) {
		if strings.HasSuffix(f.Path, "2") {
			return true, nil
		}
		return false, nil
	}

	s := FnFileScanner{
		Scanner: sliceScanner,
		Fn:      fn,
	}

	var scannedFiles []*File
	for s.Next() {
		scannedFiles = append(scannedFiles, s.File())
	}

	require.False(s.Next())
	require.NoError(s.Err())
	require.NoError(s.Close())

	require.Len(scannedFiles, 2)
}

func TestFnFileScannerErr(t *testing.T) {
	require := require.New(t)

	files := generateFiles(3)

	sliceScanner := &SliceFileScanner{Files: files}

	e := errors.New("test-error")
	fn := func(f *File) (bool, error) {
		return false, e
	}

	s := FnFileScanner{
		Scanner: sliceScanner,
		Fn:      fn,
	}

	var scannedFiles []*File
	for s.Next() {
		scannedFiles = append(scannedFiles, s.File())
	}

	require.False(s.Next())
	require.Equal(e, s.Err())
	require.NoError(s.Close())

	require.Len(scannedFiles, 0)
}

func TestFnFileScannerOnStart(t *testing.T) {
	require := require.New(t)

	files := generateFiles(3)

	sliceScanner := &SliceFileScanner{Files: files}

	fn := func(f *File) (bool, error) {
		return false, nil
	}

	var startCalled bool
	onStart := func() error {
		startCalled = true
		return nil
	}

	s := FnFileScanner{
		Scanner: sliceScanner,
		Fn:      fn,
		OnStart: onStart,
	}

	var scannedFiles []*File
	for s.Next() {
		scannedFiles = append(scannedFiles, s.File())
	}

	require.False(s.Next())
	require.NoError(s.Err())
	require.NoError(s.Close())

	require.Len(scannedFiles, 3)
	require.True(startCalled)
}

func TestFnFileScannerOnStartErr(t *testing.T) {
	require := require.New(t)

	files := generateFiles(3)

	sliceScanner := &SliceFileScanner{Files: files}

	fn := func(f *File) (bool, error) {
		return false, nil
	}

	e := errors.New("test-err")
	onStart := func() error {
		return e
	}

	s := FnFileScanner{
		Scanner: sliceScanner,
		Fn:      fn,
		OnStart: onStart,
	}

	var scannedFiles []*File
	for s.Next() {
		scannedFiles = append(scannedFiles, s.File())
	}

	require.False(s.Next())
	require.Equal(e, s.Err())
	require.NoError(s.Close())

	require.Len(scannedFiles, 0)
}

func TestFnChangeScanner(t *testing.T) {
	require := require.New(t)

	changes := generateChanges(3)

	sliceScanner := &SliceChangeScanner{Changes: changes}

	fn := func(c *Change) (bool, error) {
		if strings.HasSuffix(c.Head.Path, "2") {
			return true, nil
		}
		return false, nil
	}

	s := FnChangeScanner{
		Scanner: sliceScanner,
		Fn:      fn,
	}

	var scannedChanges []*Change
	for s.Next() {
		scannedChanges = append(scannedChanges, s.Change())
	}

	require.False(s.Next())
	require.NoError(s.Err())
	require.NoError(s.Close())

	require.Len(scannedChanges, 2)
}

func TestFnChangeScannerErr(t *testing.T) {
	require := require.New(t)

	changes := generateChanges(3)

	sliceScanner := &SliceChangeScanner{Changes: changes}

	e := errors.New("test-error")
	fn := func(f *Change) (bool, error) {
		return false, e
	}

	s := FnChangeScanner{
		Scanner: sliceScanner,
		Fn:      fn,
	}

	var scannedChanges []*Change
	for s.Next() {
		scannedChanges = append(scannedChanges, s.Change())
	}

	require.False(s.Next())
	require.Equal(e, s.Err())
	require.NoError(s.Close())

	require.Len(scannedChanges, 0)
}

func TestFnChangeScannerOnStart(t *testing.T) {
	require := require.New(t)

	changes := generateChanges(3)

	sliceScanner := &SliceChangeScanner{Changes: changes}

	fn := func(f *Change) (bool, error) {
		return false, nil
	}

	var startCalled bool
	onStart := func() error {
		startCalled = true
		return nil
	}

	s := FnChangeScanner{
		Scanner: sliceScanner,
		Fn:      fn,
		OnStart: onStart,
	}

	var scannedChanges []*Change
	for s.Next() {
		scannedChanges = append(scannedChanges, s.Change())
	}

	require.False(s.Next())
	require.NoError(s.Err())
	require.NoError(s.Close())

	require.Len(scannedChanges, 3)
	require.True(startCalled)
}

func TestFnChangeScannerOnStartErr(t *testing.T) {
	require := require.New(t)

	changes := generateChanges(3)

	sliceScanner := &SliceChangeScanner{Changes: changes}

	fn := func(f *Change) (bool, error) {
		return false, nil
	}

	e := errors.New("test-err")
	onStart := func() error {
		return e
	}

	s := FnChangeScanner{
		Scanner: sliceScanner,
		Fn:      fn,
		OnStart: onStart,
	}

	var scannedChanges []*Change
	for s.Next() {
		scannedChanges = append(scannedChanges, s.Change())
	}

	require.False(s.Next())
	require.Equal(e, s.Err())
	require.NoError(s.Close())

	require.Len(scannedChanges, 0)
}

func TestDataClientGetChanges(t *testing.T) {
	require := require.New(t)

	req := &ChangesRequest{
		Head: &ReferencePointer{
			InternalRepositoryURL: "repo",
			Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
		},
	}
	changes := generateChanges(3)
	dr := &MockService{
		T:                t,
		ExpectedCRequest: req,
		ChangeScanner:    &SliceChangeScanner{Changes: changes},
	}

	_, grpcClient := setupDataServer(t, dr)

	c := DataClient{dataClient: grpcClient}

	s, err := c.GetChanges(context.TODO(), req)
	require.NoError(err)

	var scannedChanges []*Change
	for s.Next() {
		scannedChanges = append(scannedChanges, s.Change())
	}

	require.False(s.Next())
	require.NoError(s.Err())
	require.NoError(s.Close())

	require.Len(scannedChanges, 3)
}

func TestDataClientGetFiles(t *testing.T) {
	require := require.New(t)

	req := &FilesRequest{
		Revision: &ReferencePointer{
			InternalRepositoryURL: "repo",
			Hash:                  "5262fd2b59d10e335a5c941140df16950958322d",
		},
	}
	files := generateFiles(3)
	dr := &MockService{
		T:                t,
		ExpectedFRequest: req,
		FileScanner:      &SliceFileScanner{Files: files},
	}

	_, grpcClient := setupDataServer(t, dr)

	c := DataClient{dataClient: grpcClient}

	s, err := c.GetFiles(context.TODO(), req)
	require.NoError(err)

	var scannedFiles []*File
	for s.Next() {
		scannedFiles = append(scannedFiles, s.File())
	}

	require.False(s.Next())
	require.NoError(s.Err())
	require.NoError(s.Close())

	require.Len(scannedFiles, 3)
}
