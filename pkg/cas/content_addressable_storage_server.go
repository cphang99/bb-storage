package cas

import (
	"context"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type contentAddressableStorageServer struct {
	contentAddressableStorage blobstore.BlobAccess
}

// NewContentAddressableStorageServer creates a GRPC service for serving
// the contents of a Bazel Content Addressable Storage (CAS) to Bazel.
func NewContentAddressableStorageServer(contentAddressableStorage blobstore.BlobAccess) remoteexecution.ContentAddressableStorageServer {
	return &contentAddressableStorageServer{
		contentAddressableStorage: contentAddressableStorage,
	}
}

func (s *contentAddressableStorageServer) FindMissingBlobs(ctx context.Context, in *remoteexecution.FindMissingBlobsRequest) (*remoteexecution.FindMissingBlobsResponse, error) {
	inDigests := digest.NewSetBuilder()
	for _, partialDigest := range in.BlobDigests {
		digest, err := digest.NewDigestFromPartialDigest(in.InstanceName, partialDigest)
		if err != nil {
			return nil, err
		}
		inDigests.Add(digest)
	}
	outDigests, err := s.contentAddressableStorage.FindMissing(ctx, inDigests.Build())
	if err != nil {
		return nil, err
	}
	partialDigests := make([]*remoteexecution.Digest, 0, outDigests.Length())
	for _, outDigest := range outDigests.Items() {
		partialDigests = append(partialDigests, outDigest.GetPartialDigest())
	}
	return &remoteexecution.FindMissingBlobsResponse{
		MissingBlobDigests: partialDigests,
	}, nil
}

func (s *contentAddressableStorageServer) BatchReadBlobs(ctx context.Context, in *remoteexecution.BatchReadBlobsRequest) (*remoteexecution.BatchReadBlobsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "This service does not support batched reading of blobs")
}

func (s *contentAddressableStorageServer) BatchUpdateBlobs(ctx context.Context, in *remoteexecution.BatchUpdateBlobsRequest) (*remoteexecution.BatchUpdateBlobsResponse, error) {
	// Asynchronously call Put() for every blob.
	responsesChan := make(chan *remoteexecution.BatchUpdateBlobsResponse_Response, len(in.Requests))
	for _, request := range in.Requests {
		go func(request *remoteexecution.BatchUpdateBlobsRequest_Request) {
			digest, err := digest.NewDigestFromPartialDigest(in.InstanceName, request.Digest)
			if err == nil {
				err = s.contentAddressableStorage.Put(
					ctx,
					digest,
					buffer.NewCASBufferFromByteSlice(digest, request.Data, buffer.UserProvided))
			}
			responsesChan <- &remoteexecution.BatchUpdateBlobsResponse_Response{
				Digest: request.Digest,
				Status: status.Convert(err).Proto(),
			}
		}(request)
	}

	// Recombine results.
	var response remoteexecution.BatchUpdateBlobsResponse
	for i := 0; i < len(in.Requests); i++ {
		response.Responses = append(response.Responses, <-responsesChan)
	}
	return &response, nil
}

func (s *contentAddressableStorageServer) GetTree(in *remoteexecution.GetTreeRequest, stream remoteexecution.ContentAddressableStorage_GetTreeServer) error {
	return status.Error(codes.Unimplemented, "This service does not support downloading directory trees")
}
