// Package filecopy contains the bounded, context-aware directory copy used
// when embedding local Unity packages in an acquired workspace shell.
package filecopy

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const defaultWorkers = 8

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// CopyDirParallel preserves the source provider's bounded parallel copy
// behavior. It removes dst before copying and defaults to eight workers.
func CopyDirParallel(ctx context.Context, src, dst string, workers int) error {
	if workers <= 0 {
		workers = defaultWorkers
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	type copyJob struct {
		src  string
		dst  string
		mode fs.FileMode
	}
	jobs := make(chan copyJob, workers*2)
	var wg sync.WaitGroup
	var copyErr atomic.Value
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil || copyErr.Load() != nil {
					return
				}
				if err := copyFile(ctx, job.src, job.dst, job.mode); err != nil {
					copyErr.CompareAndSwap(nil, err)
					return
				}
			}
		}()
	}
	walkErr := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if value := copyErr.Load(); value != nil {
			return value.(error)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		select {
		case jobs <- copyJob{src: path, dst: target, mode: info.Mode()}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	close(jobs)
	wg.Wait()
	if walkErr != nil {
		return walkErr
	}
	if value := copyErr.Load(); value != nil {
		return value.(error)
	}
	return nil
}

func copyFile(ctx context.Context, src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, &ctxReader{ctx: ctx, r: in}); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
