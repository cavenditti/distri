package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	// TODO: consider "github.com/klauspost/pgzip"
	gzip "compress/gzip"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"github.com/google/renameio"
	"golang.org/x/exp/mmap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
)

const installHelp = `distri install [-flags] <package>…

Install a distri package from a repository.

Example:
  % distri install i3status
`

// totalBytes counts the number of bytes written to the disk for this install
// operation.
var totalBytes int64

type errNotFound struct {
	url *url.URL
}

func (e errNotFound) Error() string {
	return fmt.Sprintf("%v: HTTP status 404", e.url)
}

type errPackageNotFound struct {
	pkg string
}

func (e errPackageNotFound) Error() string {
	return fmt.Sprintf("package %s not found on any configured repo", e.pkg)
}

func isNotExist(err error) bool {
	if _, ok := err.(*errNotFound); ok {
		return true
	}
	return os.IsNotExist(err)
}

var httpClient = &http.Client{Transport: &http.Transport{
	MaxIdleConnsPerHost: 10,
	DisableCompression:  true,
}}

type gzipReader struct {
	body io.ReadCloser
	zr   *gzip.Reader
}

func (r *gzipReader) Read(p []byte) (n int, err error) {
	return r.zr.Read(p)
}

func (r *gzipReader) Close() error {
	if err := r.zr.Close(); err != nil {
		return err
	}
	return r.body.Close()
}

func repoReader(ctx context.Context, repo distri.Repo, fn string) (io.ReadCloser, error) {
	if strings.HasPrefix(repo.Path, "http://") ||
		strings.HasPrefix(repo.Path, "https://") {
		req, err := http.NewRequest("GET", repo.Path+"/"+fn, nil) // TODO: sanitize slashes
		if err != nil {
			return nil, err
		}
		if os.Getenv("DISTRI_REEXEC") == "1" {
			req.Header.Set("X-Distri-Reexec", "yes")
		}
		// good for typical links (≤ gigabit)
		// performance bottleneck for faster links (10 gbit/s+)
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := httpClient.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			if got == http.StatusNotFound {
				return nil, &errNotFound{url: req.URL}
			}
			return nil, fmt.Errorf("%s: HTTP status %v", req.URL, resp.Status)
		}
		if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
			rd, err := gzip.NewReader(resp.Body)
			if err != nil {
				return nil, err
			}
			return &gzipReader{body: resp.Body, zr: rd}, nil
		}
		return resp.Body, nil
	}
	return os.Open(filepath.Join(repo.Path, fn))
}

func unpackDir(dest string, rd *squashfs.Reader, inode squashfs.Inode) error {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	fis, err := rd.Readdir(inode)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		destName := filepath.Join(dest, fi.Name())
		fileInode := fi.Sys().(*squashfs.FileInfo).Inode
		if fi.IsDir() {
			if err := unpackDir(destName, rd, fileInode); err != nil {
				return err
			}
		} else if fi.Mode()&os.ModeSymlink > 0 {
			target, err := rd.ReadLink(fileInode)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, destName); err != nil {
				if os.IsExist(err) {
					got, err := os.Readlink(destName)
					if err != nil {
						return err
					}
					if target != got {
						if err := os.Remove(destName); err != nil {
							log.Printf("remove(%s): %v", destName, err)
						}
						return os.Symlink(target, destName)
					}
					// fallthrough: target identical
				} else {
					return err
				}
			}
		} else if fi.Mode().IsRegular() {
			fr, err := rd.FileReader(fileInode)
			if err != nil {
				return err
			}
			f, err := os.OpenFile(destName, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
			if err != nil {
				return err
			}
			n, err := io.Copy(f, fr)
			if err != nil {
				return err
			}
			atomic.AddInt64(&totalBytes, n)
			if err := f.Close(); err != nil {
				return err
			}
		} else {
			log.Printf("ERROR: unsupported SquashFS file type: %+v", fi.Mode())
		}
	}
	return nil
}

var skipContentHooks = false

func install1(ctx context.Context, root string, repo distri.Repo, pkg string, first bool) error {
	if _, err := os.Stat(filepath.Join(root, "roimg", pkg+".squashfs")); err == nil {
		return nil // package already installed
	}

	tmpDir := filepath.Join(root, "roimg", "tmp", "."+pkg+fmt.Sprintf("%d", os.Getpid()))
	if err := os.Mkdir(tmpDir, 0755); err != nil {
		if os.IsExist(err) {
			return nil // another goroutine is installing this package
		}
		return err
	}

	log.Printf("installing package %q to root %s", pkg, root)

	for _, fn := range []string{pkg + ".squashfs", pkg + ".meta.textproto"} {
		f, err := os.Create(filepath.Join(tmpDir, fn))
		if err != nil {
			return err
		}
		in, err := repoReader(ctx, repo, "pkg/"+fn)
		if err != nil {
			return err
		}
		defer in.Close()
		n, err := io.Copy(f, in)
		if err != nil {
			return err
		}
		atomic.AddInt64(&totalBytes, n)
		in.Close()
		if err := f.Close(); err != nil {
			return err
		}
	}

	// first is true only on the first installation of the package (regardless
	// of its version).

	if first {
		readerAt, err := mmap.Open(filepath.Join(tmpDir, pkg+".squashfs"))
		if err != nil {
			return xerrors.Errorf("copying /etc: %v", err)
		}
		defer readerAt.Close()

		rd, err := squashfs.NewReader(readerAt)
		if err != nil {
			return err
		}

		fis, err := rd.Readdir(rd.RootInode())
		if err != nil {
			return err
		}
		for _, fi := range fis {
			if fi.Name() != "etc" {
				continue
			}
			log.Printf("copying %s/etc", pkg)
			if err := unpackDir(filepath.Join(root, "etc"), rd, fi.Sys().(*squashfs.FileInfo).Inode); err != nil {
				return xerrors.Errorf("copying /etc: %v", err)
			}
			break
		}
	}

	hookinstall := func(dest, src string) error {
		readerAt, err := mmap.Open(filepath.Join(tmpDir, pkg+".squashfs"))
		if err != nil {
			return xerrors.Errorf("copying %s: %v", src, err)
		}
		defer readerAt.Close()

		rd, err := squashfs.NewReader(readerAt)
		if err != nil {
			return err
		}

		inode, err := rd.LookupPath(src)
		if err != nil {
			return err
		}

		r, err := rd.FileReader(inode)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		f, err := renameio.TempFile("", dest)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			return err
		}
		if strings.HasSuffix(dest, "/init") {
			f.Chmod(0755)
		}
		if err := f.CloseAtomicallyReplace(); err != nil {
			return err
		}
		return nil
	}

	// hook: distri1
	if strings.HasPrefix(pkg, "distri1-") && distri.ParseVersion(pkg).Pkg == "distri1" {
		log.Println("hook/distri1: updating /init")
		if err := hookinstall(filepath.Join(root, "init"), "out/bin/distri"); err != nil {
			return err
		}
	}

	// hook: linux
	if strings.HasPrefix(pkg, "linux-") {
		pv := distri.ParseVersion(pkg)
		if pv.Pkg == "linux" {
			version := fmt.Sprintf("%s-%d", pv.Upstream, pv.DistriRevision)
			dest := filepath.Join(root, "boot", "vmlinuz-"+version)
			log.Printf("hook/linux: updating %s", dest)
			if err := hookinstall(dest, "out/vmlinuz"); err != nil {
				return err
			}

			if root == "/" {
				cmd := exec.Command("/etc/update-grub")
				log.Printf("hook/linux: running %v", cmd.Args)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return xerrors.Errorf("%v: %w", cmd.Args, err)
				}

				registerAtExit(func() error {
					dracut := exec.Command("sh", "-c", "dracut --force /boot/initramfs-"+pv.Upstream+"-"+strconv.FormatInt(pv.DistriRevision, 10)+".img "+pv.Upstream)
					dracut.Stderr = os.Stderr
					dracut.Stdout = os.Stdout
					log.Printf("hook/linux: running %v", dracut.Args)
					if err := dracut.Run(); err != nil {
						return xerrors.Errorf("%v: %v", dracut.Args, err)
					}
					return nil
				})
			}
		}
	}

	readerAt, err := mmap.Open(filepath.Join(tmpDir, pkg+".squashfs"))
	if err != nil {
		return err
	}
	defer readerAt.Close()

	rd, err := squashfs.NewReader(readerAt)
	if err != nil {
		return err
	}

	if !skipContentHooks {
		if _, err := rd.LookupPath("out/lib/sysusers.d"); err == nil {
			registerAtExit(func() error {
				path, err := exec.LookPath("systemd-sysusers")
				if err != nil {
					log.Printf("systemd-sysusers not found, not creating users")
					return nil
				}
				cmd := exec.Command(path, "--root="+root)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return xerrors.Errorf("%v: %v", cmd.Args, err)
				}
				return nil
			})
		}

		if _, err := rd.LookupPath("out/lib/tmpfiles.d"); err == nil {
			registerAtExit(func() error {
				path, err := exec.LookPath("systemd-tmpfiles")
				if err != nil {
					log.Printf("systemd-tmpfiles not found, not creating tmpfiles")
					return nil
				}
				cmd := exec.Command(path, "--create", "--root="+root)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return xerrors.Errorf("%v: %v", cmd.Args, err)
				}
				return nil
			})
		}
	}

	// First meta, then image: the fuse daemon considers the image canonical, so
	// it must go last.
	for _, fn := range []string{pkg + ".meta.textproto", pkg + ".squashfs"} {
		if err := os.Rename(filepath.Join(tmpDir, fn), filepath.Join(root, "roimg", fn)); err != nil {
			return err
		}
	}

	if err := os.Remove(tmpDir); err != nil {
		return err
	}

	return nil
}

func installTransitively1(root string, repos []distri.Repo, pkg string, copyEtc bool) error {
	origpkg := pkg
	if _, ok := distri.HasArchSuffix(pkg); !ok && !distri.LikelyFullySpecified(pkg) {
		pkg += "-amd64" // TODO: configurable / auto-detect
	}
	metas := make(map[*pb.Meta]distri.Repo)
	for _, repo := range repos {
		rd, err := repoReader(context.Background(), repo, "pkg/"+pkg+".meta.textproto")
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return err
		}
		b, err := ioutil.ReadAll(rd)
		rd.Close()
		if err != nil {
			return err
		}
		var pm pb.Meta
		if err := proto.UnmarshalText(string(b), &pm); err != nil {
			return err
		}
		metas[&pm] = repo
	}
	var pm *pb.Meta
	var repo distri.Repo
	for m, r := range metas {
		if pm == nil || m.GetVersion() > pm.GetVersion() {
			pm = m
			repo = r
		}
	}
	if pm == nil {
		return &errPackageNotFound{pkg: pkg}
	}

	if _, ok := distri.HasArchSuffix(pkg); ok {
		pkg += "-" + pm.GetVersion()
	}

	// TODO(later): we could write out b here and save 1 HTTP request
	pkgs := append([]string{pkg}, pm.GetRuntimeDep()...)
	log.Printf("resolved %s to %v", origpkg, pkgs)

	// download all packages with maximum concurrency for the time being
	var eg errgroup.Group
	for _, pkg := range pkgs {
		pkg := pkg //copy
		eg.Go(func() error {
			var err error
			labels := pprof.Labels("package", pkg)
			pprof.Do(context.Background(), labels, func(ctx context.Context) {
				err = install1(ctx, root, repo, pkg, copyEtc)
			})
			if err != nil {
				return fmt.Errorf("installing %s: %v", pkg, err)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	// TODO(later): tell the FUSE daemon that a (single) new package is
	// available so that new packages can be used while a bunch of them are
	// being installed?

	return nil
}

func install(args []string) error {
	fset := flag.NewFlagSet("install", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/",
			"root directory for optionally installing into a chroot")

		repo = fset.String("repo", "", "repository from which to install packages from. path (default TODO) or HTTP URL (e.g. TODO)")

		update = fset.Bool("update", false, "internal flag set by distri update, do not use")

		copyEtc = fset.Bool("copy-etc", false, "copy config files to /etc")

		//pkg = fset.String("pkg", "", "path to .squashfs package to mount")
	)
	fset.Usage = usage(fset, installHelp)
	fset.Parse(args)
	if fset.NArg() < 1 {
		return xerrors.Errorf("syntax: install [options] <package> [<package>...]")
	}

	atomic.StoreInt64(&totalBytes, 0)

	repos, err := env.Repos()
	if err != nil {
		return err
	}
	if *repo != "" {
		repos = []distri.Repo{{Path: *repo}}
	}
	if len(repos) == 0 {
		return xerrors.Errorf("no repos configured")
	}

	// TODO: lock to ensure only one process modifies roimg at a time

	tmpDir := filepath.Join(*root, "roimg", "tmp")

	// Remove stale work directories of previously interrupted/crashed processes.
	if err := os.RemoveAll(tmpDir); err != nil {
		return err
	}

	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return err
	}

	start := time.Now()
	defer func() {
		dur := time.Since(start)

		total := atomic.LoadInt64(&totalBytes)
		log.Printf("done, %.2f MB/s (%v bytes in %v)", float64(total)/1024/1024/(float64(dur)/float64(time.Second)), total, dur)
	}()

	var eg errgroup.Group
	for _, pkg := range fset.Args() {
		pkg := pkg // copy
		eg.Go(func() error {
			err := installTransitively1(*root, repos, pkg, *copyEtc)
			if _, ok := err.(*errPackageNotFound); ok && *update {
				return nil // ignore package not found
			}
			return err
		})
	}
	ctx := context.Background()
	var cl pb.FUSEClient
	eg.Go(func() error {
		// Make the FUSE daemon update its packages.
		ctl, err := os.Readlink(filepath.Join(*root, "ro", "ctl"))
		if err != nil {
			log.Printf("not updating FUSE daemon: %v", err)
			return nil // no FUSE daemon running?
		}

		log.Printf("connecting to %s", ctl)

		conn, err := grpc.DialContext(ctx, "unix://"+ctl, grpc.WithBlock(), grpc.WithInsecure())
		if err != nil {
			return err
		}
		cl = pb.NewFUSEClient(conn)
		return nil
	})
	if err := eg.Wait(); err != nil {
		return err
	}

	if cl != nil {
		if _, err := cl.ScanPackages(ctx, &pb.ScanPackagesRequest{}); err != nil {
			return err
		}
	}

	return nil
}
