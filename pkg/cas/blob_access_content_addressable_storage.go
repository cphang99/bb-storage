package cas

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"math"
	"os"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	cas_proto "github.com/buildbarn/bb-storage/pkg/proto/cas"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/golang/protobuf/proto"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type blobAccessContentAddressableStorage struct {
	blobAccess              blobstore.BlobAccess
	maximumMessageSizeBytes uint64
}

// NewBlobAccessContentAddressableStorage creates a
// ContentAddressableStorage that reads and writes Content Addressable
// Storage (CAS) objects from a BlobAccess based store.
func NewBlobAccessContentAddressableStorage(blobAccess blobstore.BlobAccess, maximumMessageSizeBytes uint64) ContentAddressableStorage {
	return &blobAccessContentAddressableStorage{
		blobAccess:              blobAccess,
		maximumMessageSizeBytes: maximumMessageSizeBytes,
	}
}

func (cas *blobAccessContentAddressableStorage) getMessage(ctx context.Context, digest *util.Digest, message proto.Message) error {
	if sizeBytes := digest.GetSizeBytes(); uint64(sizeBytes) > cas.maximumMessageSizeBytes {
		return status.Errorf(
			codes.InvalidArgument,
			"Refusing to unmarshal message of size %d, as it exceeds the maximum size of %d",
			sizeBytes, cas.maximumMessageSizeBytes)
	}
	_, r, err := cas.blobAccess.Get(ctx, digest)
	if err != nil {
		return err
	}
	data, err := ioutil.ReadAll(r)
	r.Close()
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, message)
}

func (cas *blobAccessContentAddressableStorage) GetAction(ctx context.Context, digest *util.Digest) (*remoteexecution.Action, error) {
	var action remoteexecution.Action
	if err := cas.getMessage(ctx, digest, &action); err != nil {
		return nil, err
	}
	return &action, nil
}

func (cas *blobAccessContentAddressableStorage) GetUncachedActionResult(ctx context.Context, digest *util.Digest) (*cas_proto.UncachedActionResult, error) {
	var uncachedActionResult cas_proto.UncachedActionResult
	if err := cas.getMessage(ctx, digest, &uncachedActionResult); err != nil {
		return nil, err
	}
	return &uncachedActionResult, nil
}

func (cas *blobAccessContentAddressableStorage) GetCommand(ctx context.Context, digest *util.Digest) (*remoteexecution.Command, error) {
	var command remoteexecution.Command
	if err := cas.getMessage(ctx, digest, &command); err != nil {
		return nil, err
	}
	return &command, nil
}

func (cas *blobAccessContentAddressableStorage) GetDirectory(ctx context.Context, digest *util.Digest) (*remoteexecution.Directory, error) {
	var directory remoteexecution.Directory
	if err := cas.getMessage(ctx, digest, &directory); err != nil {
		return nil, err
	}
	return &directory, nil
}

func (cas *blobAccessContentAddressableStorage) GetFile(ctx context.Context, digest *util.Digest, directory filesystem.Directory, name string, isExecutable bool) error {
	var mode os.FileMode = 0444
	if isExecutable {
		mode = 0555
	}

	w, err := directory.OpenAppend(name, filesystem.CreateExcl(mode))
	if err != nil {
		return err
	}
	defer w.Close()

	_, r, err := cas.blobAccess.Get(ctx, digest)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	r.Close()

	// Ensure no traces are left behind upon failure.
	if err != nil {
		directory.Remove(name)
	}
	return err
}

func (cas *blobAccessContentAddressableStorage) GetTree(ctx context.Context, digest *util.Digest) (*remoteexecution.Tree, error) {
	var tree remoteexecution.Tree
	if err := cas.getMessage(ctx, digest, &tree); err != nil {
		return nil, err
	}
	return &tree, nil
}

func (cas *blobAccessContentAddressableStorage) putBlob(ctx context.Context, data []byte, parentDigest *util.Digest) (*util.Digest, error) {
	// Compute new digest of data.
	digestGenerator := parentDigest.NewDigestGenerator()
	if _, err := digestGenerator.Write(data); err != nil {
		return nil, err
	}
	digest := digestGenerator.Sum()

	if err := cas.blobAccess.Put(ctx, digest, digest.GetSizeBytes(), ioutil.NopCloser(bytes.NewBuffer(data))); err != nil {
		return nil, err
	}
	return digest, nil
}

func (cas *blobAccessContentAddressableStorage) putMessage(ctx context.Context, message proto.Message, parentDigest *util.Digest) (*util.Digest, error) {
	data, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	return cas.putBlob(ctx, data, parentDigest)
}

func (cas *blobAccessContentAddressableStorage) PutFile(ctx context.Context, directory filesystem.Directory, name string, parentDigest *util.Digest) (*util.Digest, error) {
	file, err := directory.OpenRead(name)
	if err != nil {
		return nil, err
	}

	// Walk through the file to compute the digest.
	digestGenerator := parentDigest.NewDigestGenerator()
	sizeBytes, err := io.Copy(digestGenerator, io.NewSectionReader(file, 0, math.MaxInt64))
	if err != nil {
		file.Close()
		return nil, err
	}
	digest := digestGenerator.Sum()

	// Rewind and store it. Limit uploading to the size that was
	// used to compute the digest. This ensures uploads succeed,
	// even if more data gets appended in the meantime. This is not
	// uncommon, especially for stdout and stderr logs.
	if err := cas.blobAccess.Put(
		ctx,
		digest,
		digest.GetSizeBytes(),
		newSectionReadCloser(file, 0, sizeBytes)); err != nil {
		return nil, err
	}
	return digest, nil
}

// newSectionReadCloser returns an io.ReadCloser that reads from r at a
// given offset, but stops with EOF after n bytes. This function is
// identical to io.NewSectionReader(), except that it provides an
// io.ReadCloser instead of an io.Reader.
func newSectionReadCloser(r filesystem.FileReader, off int64, n int64) io.ReadCloser {
	return &struct {
		io.SectionReader
		io.Closer
	}{
		SectionReader: *io.NewSectionReader(r, off, n),
		Closer:        r,
	}
}

func (cas *blobAccessContentAddressableStorage) PutLog(ctx context.Context, log []byte, parentDigest *util.Digest) (*util.Digest, error) {
	return cas.putBlob(ctx, log, parentDigest)
}

func (cas *blobAccessContentAddressableStorage) PutTree(ctx context.Context, tree *remoteexecution.Tree, parentDigest *util.Digest) (*util.Digest, error) {
	return cas.putMessage(ctx, tree, parentDigest)
}

func (cas *blobAccessContentAddressableStorage) PutUncachedActionResult(ctx context.Context, uncachedActionResult *cas_proto.UncachedActionResult, parentDigest *util.Digest) (*util.Digest, error) {
	return cas.putMessage(ctx, uncachedActionResult, parentDigest)
}
