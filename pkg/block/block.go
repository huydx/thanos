// Package block contains common functionality for interacting with TSDB blocks
// in the context of Thanos.
package block

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"

	"fmt"

	"github.com/improbable-eng/thanos/pkg/objstore"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/tsdb"
	"github.com/prometheus/tsdb/fileutil"
)

// Meta describes the a block's meta. It wraps the known TSDB meta structure and
// extends it by Thanos-specific fields.
type Meta struct {
	Version int `json:"version"`

	tsdb.BlockMeta

	Thanos ThanosMeta `json:"thanos"`
}

// ThanosMeta holds block meta information specific to Thanos.
type ThanosMeta struct {
	Labels     map[string]string `json:"labels"`
	Downsample struct {
		Resolution int64 `json:"resolution"`
	} `json:"downsample"`
}

const (
	// MetaFilename is the known JSON filename for meta information.
	MetaFilename = "meta.json"
	// IndexFilename is the known index file for block index.
	IndexFilename = "index"
	// ChunksDirname is the known dir name for chunks with compressed samples.
	ChunksDirname = "chunks"

	// DebugMetas is a directory for debug meta files that happen in the past. Useful for debugging.
	DebugMetas = "debug/metas"
)

// WriteMetaFile writes the given meta into <dir>/meta.json.
func WriteMetaFile(dir string, meta *Meta) error {
	// Make any changes to the file appear atomic.
	path := filepath.Join(dir, MetaFilename)
	tmp := path + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(f)
	enc.SetIndent("", "\t")

	if err := enc.Encode(meta); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return renameFile(tmp, path)
}

// ReadMetaFile reads the given meta from <dir>/meta.json.
func ReadMetaFile(dir string) (*Meta, error) {
	b, err := ioutil.ReadFile(filepath.Join(dir, MetaFilename))
	if err != nil {
		return nil, err
	}
	var m Meta

	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Version != 1 {
		return nil, errors.Errorf("unexpected meta file version %d", m.Version)
	}
	return &m, nil
}

func renameFile(from, to string) error {
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}

	// Directory was renamed; sync parent dir to persist rename.
	pdir, err := fileutil.OpenDir(filepath.Dir(to))
	if err != nil {
		return err
	}

	if err = fileutil.Fsync(pdir); err != nil {
		pdir.Close()
		return err
	}
	return pdir.Close()
}

// Download downloads directory that is mean to be block directory.
func Download(ctx context.Context, bucket objstore.Bucket, id ulid.ULID, dst string) error {
	if err := objstore.DownloadDir(ctx, bucket, id.String(), dst); err != nil {
		return err
	}

	chunksDir := filepath.Join(dst, ChunksDirname)
	_, err := os.Stat(chunksDir)
	if os.IsNotExist(err) {
		// This can happen if block is empty. We cannot easily upload empty directory, so create one here.
		return os.Mkdir(chunksDir, os.ModePerm)
	}

	if err != nil {
		return errors.Wrapf(err, "stat %s", chunksDir)
	}

	return nil
}

// Upload uploads block from given block dir that ends with block id.
// It makes sure cleanup is done on error to avoid partial block uploads.
// It also verifies basic features of Thanos block.
// TODO(bplotka): Ensure bucket operations have reasonable backoff retries.
func Upload(ctx context.Context, bkt objstore.Bucket, bdir string) error {
	df, err := os.Stat(bdir)
	if err != nil {
		return errors.Wrap(err, "stat bdir")
	}
	if !df.IsDir() {
		return errors.Errorf("%s is not a directory", bdir)
	}

	// Verify dir.
	id, err := ulid.Parse(df.Name())
	if err != nil {
		return errors.Wrap(err, "not a block dir")
	}

	meta, err := ReadMetaFile(bdir)
	if err != nil {
		// No meta or broken meta file.
		return errors.Wrap(err, "read meta")
	}

	if meta.Thanos.Labels == nil || len(meta.Thanos.Labels) == 0 {
		return errors.Errorf("empty external labels are not allowed for Thanos block.")
	}

	if objstore.UploadFile(ctx, bkt, path.Join(bdir, MetaFilename), path.Join(DebugMetas, fmt.Sprintf("%s.json", id))); err != nil {
		return errors.Wrap(err, "upload meta file to debug dir")
	}

	if err := objstore.UploadDir(ctx, bkt, path.Join(bdir, ChunksDirname), path.Join(id.String(), ChunksDirname)); err != nil {
		return cleanUp(bkt, id, errors.Wrap(err, "upload chunks"))
	}

	if objstore.UploadFile(ctx, bkt, path.Join(bdir, IndexFilename), path.Join(id.String(), IndexFilename)); err != nil {
		return cleanUp(bkt, id, errors.Wrap(err, "upload index"))
	}

	// Meta.json always need to be uploaded as a last item. This will allow to assume block directories without meta file
	// to be pending uploads.
	if objstore.UploadFile(ctx, bkt, path.Join(bdir, MetaFilename), path.Join(id.String(), MetaFilename)); err != nil {
		return cleanUp(bkt, id, errors.Wrap(err, "upload meta file"))
	}

	return nil
}

func cleanUp(bkt objstore.Bucket, id ulid.ULID, err error) error {
	// Cleanup the dir with an uncancelable context.
	cleanErr := Delete(context.Background(), bkt, id)
	if cleanErr != nil {
		return errors.Wrapf(err, "failed to clean block after upload issue. Partial block in system. Err: %s", err.Error())
	}
	return err
}

// Delete removes directory that is mean to be block directory.
// NOTE: Prefer this method instead of objstore.Delete to avoid deleting empty dir (whole bucket) by mistake.
func Delete(ctx context.Context, bucket objstore.Bucket, id ulid.ULID) error {
	return objstore.DeleteDir(ctx, bucket, id.String())
}

// DownloadMeta downloads only meta file from bucket by block ID.
func DownloadMeta(ctx context.Context, bkt objstore.Bucket, id ulid.ULID) (Meta, error) {
	rc, err := bkt.Get(ctx, path.Join(id.String(), MetaFilename))
	if err != nil {
		return Meta{}, errors.Wrapf(err, "meta.json bkt get for %s", id.String())
	}
	defer rc.Close()

	var m Meta
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		return Meta{}, errors.Wrapf(err, "decode meta.json for block %s", id.String())
	}
	return m, nil
}

func IsBlockDir(path string) (id ulid.ULID, ok bool) {
	id, err := ulid.Parse(filepath.Base(path))
	return id, err == nil
}

// Finalize sets Thanos meta to the block meta JSON and saves it to the disk. It also removes tombstones which are not
// useful for Thanos.
// NOTE: It should be used after writing any block by any Thanos component, otherwise we will miss crucial metadata.
func Finalize(bdir string, extLset map[string]string, resolution int64, downsampledMeta *tsdb.BlockMeta) (*Meta, error) {
	newMeta, err := ReadMetaFile(bdir)
	if err != nil {
		return nil, errors.Wrap(err, "read new meta")
	}
	newMeta.Thanos.Labels = extLset
	newMeta.Thanos.Downsample.Resolution = resolution

	// While downsampling we need to copy original compaction.
	if downsampledMeta != nil {
		newMeta.Compaction = downsampledMeta.Compaction
	}

	if err := WriteMetaFile(bdir, newMeta); err != nil {
		return nil, errors.Wrap(err, "write new meta")
	}

	if err = os.Remove(filepath.Join(bdir, "tombstones")); err != nil {
		return nil, errors.Wrap(err, "remove tombstones")
	}

	return newMeta, nil
}
