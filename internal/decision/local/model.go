package local

// The weights are a build artifact rather than a pip install.
//
// Converting the published checkpoint to the int8 ONNX this runtime serves
// needs torch and transformers — some gigabytes of exactly what the runtime
// exists to avoid. So the conversion happens once, off the user's machine,
// and what ships is the result: a tarball published as a release asset and
// fetched the way Factor fetches its own binary. The URL is built rather than
// looked up, so nothing here spends an api.github.com request, and the
// checksum is checked before anything is unpacked, because a half-downloaded
// model is a server that starts and then answers nonsense.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ModelTag is the release the weights are published under. It moves only
	// when the checkpoint or the conversion does, which is why it is not the
	// Factor version: a release every few weeks must not re-download 250 MB.
	ModelTag = "model-laya-multilingual-int8-v1"
	// ModelAsset is the file under that tag.
	ModelAsset = "laya-multilingual-int8.tar.gz"
	// ModelSHA256 is what it must hash to. Built from
	// convaiinnovations/laya-multilingual with `edgejev build --precision int8`.
	ModelSHA256 = "a724d4afc4d0a51e128b01b78cf64a5d285817c71c45918c2c4780658722d943"
	// maxModelBytes bounds what will be written to disk from that URL. The
	// artifact is around 250 MB; anything an order of magnitude past it is
	// not the model.
	maxModelBytes = 2 << 30
)

// Seams: the artifact can be served and checked without a release behind it.
var (
	// modelBase is the release download root; releases are public, so this
	// needs no token and no API call.
	modelBase = "https://github.com/cyqlelabs/factor/releases/download"
	// modelSum is what the artifact must hash to.
	modelSum = ModelSHA256
)

// ModelDir is where the unpacked model lives, beside the virtualenv that
// serves it.
func ModelDir(home string) string { return filepath.Join(home, "decision-model") }

func modelConfigPath(home string) string { return filepath.Join(ModelDir(home), "edgejev.json") }

// modelConfig is the part of the model's own metadata Factor reads: the
// window the checkpoint was built with, which is what a caller has to size
// its questions against.
type modelConfig struct {
	MaxLen     int    `json:"max_len"`
	HeadMaxLen int    `json:"head_max_len"`
	ONNXFile   string `json:"onnx_file"`
	Precision  string `json:"precision"`
	Source     string `json:"source_model"`
}

// readModelConfig reads that metadata, or says it could not.
func readModelConfig(home string) (modelConfig, bool) {
	raw, err := os.ReadFile(modelConfigPath(home))
	if err != nil {
		return modelConfig{}, false
	}
	var cfg modelConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return modelConfig{}, false
	}
	return cfg, cfg.MaxLen > 0
}

// ModelReady reports whether a usable model is unpacked here. The metadata
// and the graph both have to be there: a tarball interrupted mid-extract
// leaves one without the other.
func ModelReady(home string) bool {
	cfg, ok := readModelConfig(home)
	if !ok {
		return false
	}
	name := cfg.ONNXFile
	if name == "" {
		name = "model.onnx"
	}
	info, err := os.Stat(filepath.Join(ModelDir(home), name))
	return err == nil && info.Size() > 0
}

// ensureModel downloads and unpacks the weights when they are not already
// here. It is safe to call on every install: a model that is ready is left
// alone.
func ensureModel(ctx context.Context, home string, emit func(string, ...any)) error {
	if ModelReady(home) {
		return nil
	}
	url := modelBase + "/" + ModelTag + "/" + ModelAsset
	emit("fetching the decision model (about 250 MB)…")

	tmp, err := os.CreateTemp(filepath.Dir(ModelDir(home)), "decision-model-*.tar.gz")
	if err != nil {
		return fmt.Errorf("could not stage the decision model: %w", err)
	}
	staged := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(staged)
	}()

	sum, err := fetch(ctx, url, tmp)
	if err != nil {
		return err
	}
	if sum != modelSum {
		return fmt.Errorf("the decision model did not match its checksum (got %s); nothing was unpacked", sum)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// Unpacked beside the destination and renamed into place, so an
	// interrupted extract never leaves a directory that looks installed.
	dir, err := os.MkdirTemp(filepath.Dir(ModelDir(home)), "decision-model-unpack-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := untar(tmp, dir); err != nil {
		return fmt.Errorf("unpacking the decision model: %w", err)
	}
	_ = os.RemoveAll(ModelDir(home))
	if err := os.Rename(dir, ModelDir(home)); err != nil {
		return fmt.Errorf("installing the decision model: %w", err)
	}
	if !ModelReady(home) {
		return fmt.Errorf("the decision model unpacked without %s", filepath.Base(modelConfigPath(home)))
	}
	emit("the decision model is in place (%s)", ModelDir(home))
	return nil
}

// fetch streams a URL into w and returns what it hashed to.
func fetch(ctx context.Context, url string, w io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching the decision model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching the decision model: %s said %s", url, resp.Status)
	}
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, sum), io.LimitReader(resp.Body, maxModelBytes)); err != nil {
		return "", fmt.Errorf("fetching the decision model: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// untar unpacks a gzipped tar into dir, refusing any entry that would land
// outside it. The archive is one Factor publishes, but an extractor that
// trusts its input is the bug that is worth not having.
func untar(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		head, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dir, head.Name)
		if err != nil {
			return err
		}
		switch head.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, maxModelBytes)); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Symlinks and devices have no business in a model archive.
			return fmt.Errorf("%s carries an entry that is not a file or a directory", ModelAsset)
		}
	}
}

// safeJoin resolves an archive entry against dir and refuses anything that
// climbs out of it.
func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%s carries an entry outside the archive: %q", ModelAsset, name)
	}
	return filepath.Join(dir, clean), nil
}
