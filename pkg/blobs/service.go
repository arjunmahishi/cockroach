// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

/*
Package blobs contains a gRPC service to be used for remote file access.

It is used for bulk file reads and writes to files on any CockroachDB node.
Each node will run a blob service, which serves the file access for files on
that node. Each node will also have a blob client, which uses the nodedialer
to connect to another node's blob service, and access its files. The blob client
is the point of entry to this service and it supports the `BlobClient` interface,
which includes the following functionalities:
  - ReadFile
  - WriteFile
  - List
  - Delete
  - Stat
*/
package blobs

import (
	"context"
	"io"

	"github.com/cockroachdb/cockroach/pkg/blobs/blobspb"
	"github.com/cockroachdb/cockroach/pkg/util/grpcutil"
	"github.com/cockroachdb/cockroach/pkg/util/ioctx"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/oserror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements the gRPC BlobService which exchanges bulk files between different nodes.
type Service struct {
	localStorage *LocalStorage
}

var _ blobspb.BlobServer = &Service{}

// NewBlobService instantiates a blob service server.
func NewBlobService(externalIODir string) (*Service, error) {
	localStorage, err := NewLocalStorage(externalIODir)
	return &Service{localStorage: localStorage}, err
}

// GetStream implements the gRPC service.
func (s *Service) GetStream(req *blobspb.GetRequest, stream blobspb.Blob_GetStreamServer) error {
	content, _, err := s.localStorage.ReadFile(req.Filename, req.Offset)
	if err != nil {
		return err
	}
	defer content.Close(stream.Context())
	return streamContent(stream.Context(), stream, content)
}

// PutStream implements the gRPC service.
func (s *Service) PutStream(stream blobspb.Blob_PutStreamServer) error {
	filename, ok := grpcutil.FastFirstValueFromIncomingContext(stream.Context(), "filename")
	if !ok {
		return errors.New("could not fetch metadata or no filename in metadata")
	}
	if filename == "" {
		return errors.New("invalid filename in metadata")
	}

	reader := newPutStreamReader(stream)
	defer reader.Close(stream.Context())
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	w, err := s.localStorage.Writer(ctx, filename)
	if err != nil {
		cancel()
		return err
	}

	if _, err := io.Copy(w, ioctx.ReaderCtxAdapter(stream.Context(), reader)); err != nil {
		cancel()
		return errors.CombineErrors(err, w.Close())
	}
	err = w.Close()
	cancel()
	return err
}

// List implements the gRPC service.
func (s *Service) List(
	ctx context.Context, req *blobspb.GlobRequest,
) (*blobspb.GlobResponse, error) {
	matches, err := s.localStorage.List(req.Pattern)
	return &blobspb.GlobResponse{Files: matches}, err
}

// Delete implements the gRPC service.
func (s *Service) Delete(
	ctx context.Context, req *blobspb.DeleteRequest,
) (*blobspb.DeleteResponse, error) {
	return &blobspb.DeleteResponse{}, s.localStorage.Delete(req.Filename)
}

// Stat implements the gRPC service.
func (s *Service) Stat(ctx context.Context, req *blobspb.StatRequest) (*blobspb.BlobStat, error) {
	resp, err := s.localStorage.Stat(req.Filename)
	if oserror.IsNotExist(err) {
		// gRPC hides the underlying golang ErrNotExist error, so we send back an
		// equivalent gRPC error which can be handled gracefully on the client side.
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return resp, err
}
