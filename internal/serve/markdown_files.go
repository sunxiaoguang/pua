package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/disksing/pua/internal/app"
)

const markdownSaveRequestMaxBytes = previewMaxBytes + 64*1024

var errMarkdownContentConflict = errors.New("Markdown file changed on disk")

func (s *server) saveResourceMarkdownFile(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}

	relPath := filepath.ToSlash(filepath.Clean(strings.TrimSpace(r.URL.Query().Get("path"))))
	if relPath == "." || relPath == "" {
		writeError(w, errors.New("path is required"), http.StatusBadRequest)
		return
	}
	var body struct {
		Content             string `json:"content"`
		ExpectedContentHash string `json:"expectedContentHash"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, markdownSaveRequestMaxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.ExpectedContentHash) == "" {
		writeError(w, errors.New("expectedContentHash is required"), http.StatusBadRequest)
		return
	}
	content := []byte(body.Content)
	if len(content) > previewMaxBytes {
		writeError(w, fmt.Errorf("Markdown files larger than %d bytes cannot be edited", previewMaxBytes), http.StatusRequestEntityTooLarge)
		return
	}
	if !utf8.Valid(content) || containsNUL(content) {
		writeError(w, errors.New("Markdown content must be valid UTF-8 text"), http.StatusBadRequest)
		return
	}

	var abs string
	errorStatus := http.StatusBadRequest
	err = s.withWorkspaceMutation(r.Context(), workspace, resourceID, func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		resource, resourceErr := puaWorkspace.ResourceValue(resourceID)
		if resourceErr != nil {
			errorStatus = http.StatusNotFound
			return resourceErr
		}
		if resource.Archived {
			return errors.New("archived resources cannot be edited")
		}
		abs, resourceErr = editableResourceMarkdownPath(current.Path, resource, relPath)
		if resourceErr != nil {
			return resourceErr
		}
		if resourceErr = replaceMarkdownFile(abs, content, body.ExpectedContentHash); resourceErr != nil {
			if errors.Is(resourceErr, errMarkdownContentConflict) {
				errorStatus = http.StatusConflict
				return errors.New("Markdown file changed on disk; reconcile the preserved browser draft before saving")
			}
			errorStatus = http.StatusInternalServerError
			return resourceErr
		}
		return nil
	})
	if err != nil {
		writeError(w, err, errorStatus)
		return
	}
	previewPath(w, relPath, abs)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("request body must contain exactly one JSON value")
}

func editableResourceMarkdownPath(workspaceRoot string, resource app.ResourceResult, relPath string) (string, error) {
	if !isMarkdownFilePath(relPath) {
		return "", errors.New("only Markdown files can be edited")
	}
	resourceRel := filepath.ToSlash(filepath.Clean(resource.Path))
	pathRel, err := filepath.Rel(filepath.FromSlash(resourceRel), filepath.FromSlash(relPath))
	if err != nil || pathRel == ".." || strings.HasPrefix(pathRel, ".."+string(filepath.Separator)) {
		return "", errors.New("file does not belong to the selected resource")
	}
	pathSlash := filepath.ToSlash(pathRel)
	allowed := false
	switch {
	case resource.Project != nil && pathSlash == "project.md":
		allowed = true
	case resource.Task != nil && pathSlash == "task.md":
		allowed = true
	case resource.Project != nil && strings.HasPrefix(pathSlash, "templates/") && !strings.Contains(strings.TrimPrefix(pathSlash, "templates/"), "/"):
		allowed = true
	case strings.HasPrefix(pathSlash, "artifacts/"):
		allowed = true
	}
	if !allowed {
		return "", errors.New("file is outside the editable Markdown locations for this resource")
	}

	abs, err := safeWorkspacePath(workspaceRoot, filepath.FromSlash(relPath))
	if err != nil {
		return "", err
	}
	resourceAbs, err := safeWorkspacePath(workspaceRoot, filepath.FromSlash(resourceRel))
	if err != nil {
		return "", err
	}
	resourceEval, err := filepath.EvalSymlinks(resourceAbs)
	if err != nil {
		return "", err
	}
	targetEval, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New("only existing Markdown files can be edited")
		}
		return "", err
	}
	if err := ensurePathInside(resourceEval, targetEval); err != nil {
		return "", errors.New("file symlink escapes the selected resource")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("editable Markdown path must be a regular file, not a symbolic link")
	}
	if info.Size() > previewMaxBytes {
		return "", fmt.Errorf("Markdown files larger than %d bytes cannot be edited", previewMaxBytes)
	}
	return abs, nil
}

func isMarkdownFilePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".mdown", ".mkdn":
		return true
	default:
		return false
	}
}

func replaceMarkdownFile(path string, content []byte, expectedHash string) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if previewContentHash(current) != expectedHash {
		return errMarkdownContentConflict
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	temp, err := os.CreateTemp(filepath.Dir(path), ".pua-markdown-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	closeWithError := func(cause error) error {
		if closeErr := temp.Close(); cause == nil {
			return closeErr
		}
		return cause
	}
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		return closeWithError(err)
	}
	if _, err := temp.Write(content); err != nil {
		return closeWithError(err)
	}
	if err := temp.Sync(); err != nil {
		return closeWithError(err)
	}
	if err := temp.Close(); err != nil {
		return err
	}

	latest, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if previewContentHash(latest) != expectedHash {
		return errMarkdownContentConflict
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
