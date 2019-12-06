package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	cmdfuse "github.com/distr1/distri/cmd/distri/internal/fuse"
	"github.com/distr1/distri/internal/env"
	"github.com/jacobsa/fuse"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
)

const packHelp = `distri pack [-flags]

Pack a distri system image (for a USB memory stick, qemu, cloud, …).

This command is typically invoked through the distri Makefile:

Example:
  % make image serial=1
  % make qemu-serial
`

const passwd = `root:x:0:0:root:/root:/bin/sh
`
const group = `root:x:0:
`

func copyFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return nil
}

type packctx struct {
	root           string
	repo           string
	extraBase      string
	diskImg        string
	gcsDiskImg     string
	encrypt        bool
	serialOnly     bool
	bootDebug      bool
	branch         string
	rootPassword   string
	cryptPassword  string
	docker         bool
	authorizedKeys string
}

func pack(args []string) error {
	fset := flag.NewFlagSet("pack", flag.ExitOnError)
	var p packctx
	fset.StringVar(&p.root, "root",
		"",
		"TODO")
	fset.StringVar(&p.repo, "repo", env.DefaultRepoRoot, "TODO")
	fset.StringVar(&p.extraBase, "base", "", "if non-empty, an additional base image to install")
	fset.StringVar(&p.diskImg, "diskimg", "", "Write a btrfs file system image to the specified path")
	fset.StringVar(&p.gcsDiskImg, "gcsdiskimg", "", "Write a Google Cloud file system image (tar.gz containing disk.raw) to the specified path")
	fset.BoolVar(&p.encrypt, "encrypt", false, "Whether to encrypt the image’s partitions (with LUKS)")
	fset.BoolVar(&p.serialOnly, "serialonly", false, "Whether to print output only on console=ttyS0,115200 (defaults to false, i.e. console=tty1)")
	fset.BoolVar(&p.bootDebug, "bootdebug", false, "Whether to debug early boot, i.e. add systemd.log_level=debug systemd.log_target=console")
	fset.StringVar(&p.branch, "branch", "master", "Which git branch to track in repo URL")
	fset.StringVar(&p.rootPassword, "root_password", "peace", "password to set for the root account")
	fset.StringVar(&p.cryptPassword, "crypt_password", "peace", "disk encryption password to use with -encrypt")
	fset.BoolVar(&p.docker, "docker", false, "generate a tar ball to feed to docker import")
	fset.StringVar(&p.authorizedKeys, "authorized_keys", "", "if non-empty, path to an SSH authorized_keys file to include for the root user")
	fset.Usage = usage(fset, packHelp)
	fset.Parse(args)

	if p.gcsDiskImg == "" && p.diskImg == "" && !p.docker {
		if p.root == "" {
			return xerrors.Errorf("syntax: pack -root=<directory>")
		}

		if err := p.pack(p.root); err != nil {
			return err
		}
	}

	if p.gcsDiskImg != "" && p.diskImg == "" {
		// Creating a Google Cloud disk image requires creating a disk image
		// first, so use a temporary file:
		tmp, err := ioutil.TempFile("", "distriimg")
		if err != nil {
			return err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		p.diskImg = tmp.Name()
	}

	if p.diskImg != "" {
		if err := p.writeDiskImg(); err != nil {
			return xerrors.Errorf("writeDiskImg: %v", err)
		}
	}

	if p.gcsDiskImg != "" {
		log.Printf("Writing Google Cloud disk image to %s", p.gcsDiskImg)
		img, err := os.Open(p.diskImg)
		if err != nil {
			return err
		}
		defer img.Close()
		st, err := img.Stat()
		if err != nil {
			return err
		}

		f, err := os.Create(p.gcsDiskImg)
		if err != nil {
			return err
		}
		defer f.Close()
		gw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
		if err != nil {
			return err
		}
		tw := tar.NewWriter(gw)
		if err := tw.WriteHeader(&tar.Header{
			Name:   "disk.raw",
			Size:   st.Size(),
			Mode:   0644,
			Format: tar.FormatGNU,
		}); err != nil {
			return err
		}
		if _, err := io.Copy(tw, img); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
		if err := gw.Close(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}

	if p.docker {
		root, err := ioutil.TempDir("", "distridocker")
		if err != nil {
			return err
		}
		defer os.RemoveAll(root)

		skipContentHooks = true
		if err := install(append(
			[]string{
				"-root=" + root,
				"-repo=" + p.repo,
			},
			"base",
			"rxvt-unicode",    // for its terminfo file
			"ca-certificates", // so that we can install packages via https
		)); err != nil {
			return err
		}

		for _, dir := range []string{
			"etc",
			"etc/distri/repos.d",
			"ro",
			"ro-tmp",
		} {
			if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
				return err
			}
		}

		if err := ioutil.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd), 0644); err != nil {
			return err
		}

		if err := ioutil.WriteFile(filepath.Join(root, "etc/distri/repos.d/distr1.repo"), []byte("https://repo.distr1.org/distri/"+p.branch+"\n"), 0644); err != nil {
			return err
		}

		type symlink struct {
			oldname, newname string
		}
		for _, link := range []symlink{
			{"/", "usr"},
			{"/ro/bin", "bin"},
			{"/ro/share", "share"},
			{"/ro/lib", "lib"},
			{"/ro/include", "include"},
			{"/ro/sbin", "sbin"},
			{"/init", "entrypoint"},
		} {
			if err := os.Symlink(link.oldname, filepath.Join(root, link.newname)); err != nil {
				return err
			}
		}

		// Remove packages we don’t need to reduce docker container size:
		b := &buildctx{Arch: "amd64"} // TODO: introduce a packctx, make glob take a common ctx
		resolved, err := b.glob(filepath.Join(p.repo, "pkg"), []string{
			"linux-firmware",
			"docker-engine",
			"dracut",
			"binutils",
			"elfutils",
		})
		if err != nil {
			return err
		}

		for _, pkg := range resolved {
			for _, ext := range []string{"squashfs", "meta.textproto"} {
				if err := os.Remove(filepath.Join(root, "roimg", pkg+"."+ext)); err != nil {
					return err
				}
			}
		}

		tar := exec.Command("tar", "-c", ".")
		tar.Dir = root
		tar.Stdout = os.Stdout
		tar.Stderr = os.Stderr
		if err := tar.Run(); err != nil {
			return fmt.Errorf("%v: %v", tar.Args, err)
		}
	}

	return nil
}

func (p *packctx) pack(root string) error {
	for _, dir := range []string{
		"etc",
		"root",
		"boot", // grub
		//"esp",     // grub (EFI System Partition)
		"dev",     // udev
		"ro",      // read-only package directory (mountpoint)
		"ro-dbg",  // read-only package directory (mountpoint)
		"roimg",   // read-only package store
		"rodebug", // read-only package store
		"ro-tmp",  // temporary directory which is not hidden by systemd’s tmp.mount
		"proc",    // procfs
		"sys",     // sysfs
		"tmp",     // tmpfs
		"var/tmp", // systemd (e.g. systemd-networkd)
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			return err
		}
	}

	if err := os.Symlink("/run", filepath.Join(root, "var", "run")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/bin", filepath.Join(root, "bin")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/bin", filepath.Join(root, "sbin")); err != nil && !os.IsExist(err) {
		return err
	}

	// We run systemd in non-split mode, so /usr needs to point to /
	if err := os.Symlink("/", filepath.Join(root, "usr")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/lib", filepath.Join(root, "lib")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/share", filepath.Join(root, "share")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink("/ro/include", filepath.Join(root, "include")); err != nil && !os.IsExist(err) {
		return err
	}

	// TODO: de-duplicate with build.go
	if err := os.Symlink("/ro/glibc-amd64-2.27-3/out/lib", filepath.Join(root, "lib64")); err != nil && !os.IsExist(err) {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(root, "etc/resolv.conf"), []byte("nameserver 8.8.8.8\nnameserver 2001:4860:4860::8888\n"), 0644); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd), 0644); err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(root, "etc/group"), []byte(group), 0644); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(root, "etc/distri/repos.d"), 0755); err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(root, "etc/distri/repos.d/distr1.repo"), []byte("https://repo.distr1.org/distri/"+p.branch+"\n"), 0644); err != nil {
		return err
	}

	if p.authorizedKeys != "" {
		if err := os.MkdirAll(filepath.Join(root, "root/.ssh"), 0700); err != nil {
			return err
		}
		if err := copyFile(p.authorizedKeys, filepath.Join(root, "root/.ssh/authorized_keys")); err != nil {
			return err
		}
	}

	b := &buildctx{Arch: "amd64"} // TODO: introduce a packctx, make glob take a common ctx

	basePkgNames := []string{"base"} // contains packages required for pack
	if p.extraBase != "" {
		basePkgNames = append(basePkgNames, p.extraBase)
		pkgset := filepath.Join(root, "etc", "distri", "pkgset.d", "extrabase.pkgset")
		if err := os.MkdirAll(filepath.Dir(pkgset), 0755); err != nil {
			return err
		}
		if err := ioutil.WriteFile(pkgset, []byte(p.extraBase+"\n"), 0644); err != nil {
			return err
		}
	}

	basePkgs, err := b.glob(filepath.Join(p.repo, "pkg"), basePkgNames)
	if err != nil {
		return err
	}

	skipContentHooks = true
	if err := install(append([]string{
		"-root=" + root,
		"-repo=" + p.repo,
	}, basePkgs...)); err != nil {
		return err
	}

	if _, err := cmdfuse.Mount([]string{"-repo=" + filepath.Join(root, "roimg"), filepath.Join(root, "ro")}); err != nil {
		return err
	}
	defer fuse.Unmount(filepath.Join(root, "ro"))

	// XXX: this is required for systemd-firstboot
	cmdline := filepath.Join(root, "proc", "cmdline")
	if err := ioutil.WriteFile(cmdline, []byte("systemd.firstboot=1"), 0644); err != nil {
		return err
	}

	//copy /ro/etc content to /etc for first boot
	if err := moveContent(filepath.Join(root, "ro/etc"), filepath.Join(root, "etc")); err != nil {
		return err
	}
	// if err := copyContent(filepath.Join(root, "ro/etc"), filepath.Join(root, "etc")); err != nil {
	// 	return err
	// }

	defer os.Remove(cmdline)
	cmd := exec.Command("unshare",
		"--user",
		"--map-root-user", // for mount permissions in the namespace
		"--mount",
		"--",
		"chroot", root, "/ro/systemd-amd64-239-10/bin/systemd-firstboot", "--hostname=distri0",
		"--root-password="+p.rootPassword,
		"--copy-timezone",
		"--copy-locale",
		"--setup-machine-id")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v: %w", cmd.Args, err)
	}

	cmd = exec.Command("unshare",
		"--user",
		"--map-root-user", // for mount permissions in the namespace
		"--mount",
		"--",
		"chroot", root, "/ro/systemd-amd64-239-10/bin/systemd-sysusers",
		"/ro/systemd-amd64-239-10/out/lib/sysusers.d/basic.conf",
		"/ro/systemd-amd64-239-10/out/lib/sysusers.d/systemd.conf")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v: %w", cmd.Args, err)
	}

	// TODO: dynamically find which units to enable (test: xdm)
	units := []string{
		"systemd-networkd",
		"containerd",
		"docker",
		"ssh",
		"haveged",
	}
	if p.extraBase == "base-x11" {
		units = append(units, "debugfs")
	}
	cmd = exec.Command("unshare",
		append([]string{
			"--user",
			"--map-root-user", // for mount permissions in the namespace
			"--mount",
			"--",
			"chroot", root, "/ro/systemd-amd64-239-10/bin/systemctl",
			"enable",
		}, units...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("%v: %w", cmd.Args, err)
	}

	pamd := filepath.Join(root, "etc", "pam.d")
	if err := os.MkdirAll(pamd, 0755); err != nil {
		return err
	}

	const pamdOther = `auth	required	pam_unix.so
auth	required	pam_warn.so
account	required	pam_unix.so
account	required	pam_warn.so

# success=1 will skip the pam_warn.so line
password	[success=1 default=ignore]	pam_unix.so
password	requisite	pam_warn.so
password	required	pam_permit.so

session	required	pam_unix.so
session	optional	pam_systemd.so
session	required	pam_warn.so
`
	if err := ioutil.WriteFile(filepath.Join(pamd, "other"), []byte(pamdOther), 0644); err != nil {
		return err
	}
	if err := os.Symlink("other", filepath.Join(pamd, "system-auth")); err != nil && !os.IsExist(err) {
		return err
	}

	const dbusSystemLocal = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <includedir>/ro/share/dbus-1/system.d</includedir>
</busconfig>
`
	if err := ioutil.WriteFile(filepath.Join(root, "etc", "dbus-1", "system-local.conf"), []byte(dbusSystemLocal), 0644); err != nil {
		return err
	}

	const nsswitch = `passwd:         compat mymachines systemd
group:          compat mymachines systemd
shadow:         compat

hosts:          files mymachines resolve [!UNAVAIL=return] dns  myhostname
networks:       files

protocols:      db files
services:       db files
ethers:         db files
rpc:            db files

netgroup:       nis
`
	if err := ioutil.WriteFile(filepath.Join(root, "etc", "nsswitch.conf"), []byte(nsswitch), 0644); err != nil {
		return err
	}

	if err := adduser(root, "systemd-network:x:101:101:network:/run/systemd/netif:/bin/false"); err != nil {
		return err
	}
	if err := addgroup(root, "systemd-network:x:103:"); err != nil {
		return err
	}
	if err := adduser(root, "systemd-resolve:x:105:105:resolve:/run/systemd/resolve:/bin/false"); err != nil {
		return err
	}
	if err := addgroup(root, "systemd-resolve:x:105:"); err != nil {
		return err
	}

	if err := adduser(root, "sshd:x:102:102:sshd:/:/bin/false"); err != nil {
		return err
	}

	if err := adduser(root, "messagebus:x:106:106:messagebus:/var/run/dbus:/bin/false"); err != nil {
		return err
	}

	if err := addgroup(root, "docker:x:104:"); err != nil {
		return err
	}

	if err := addgroup(root, "messagebus:x:106:"); err != nil {
		return err
	}

	// TODO: once https://github.com/systemd/systemd/issues/3998 is fixed, use
	// their catch-all file rather than ours.
	network := filepath.Join(root, "etc", "systemd", "network")
	if err := os.MkdirAll(network, 0755); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(network, "en.network"), []byte(`
[Match]
#Type=ether
Name=en*

[Network]
DHCP=yes
`), 0644); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(network, "eth.network"), []byte(`
[Match]
#Type=ether
Name=eth*

[Network]
DHCP=yes
`), 0644); err != nil {
		return err
	}

	modules := filepath.Join(root, "etc", "modules-load.d")
	if err := os.MkdirAll(modules, 0755); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(modules, "docker.conf"), []byte(`
iptable_nat
ipt_MASQUERADE
xt_addrtype
veth
`), 0644); err != nil {
		return err
	}

	fuse.Unmount(filepath.Join(root, "ro"))

	chown := exec.Command("sh", "-c", fmt.Sprintf(`find "%s" -not -path "/mnt/boot*" -xdev -print0 | sudo xargs -0 chown --no-dereference --from="%s" root:root`, root, os.Getenv("USER")))
	chown.Stderr = os.Stderr
	chown.Stdout = os.Stdout
	if err := chown.Run(); err != nil {
		return xerrors.Errorf("%v: %v", chown.Args, err)
	}

	return nil
}

func (p *packctx) writeDiskImg() error {
	f, err := os.OpenFile(p.diskImg, os.O_CREATE|os.O_TRUNC|os.O_RDWR|unix.O_CLOEXEC, 0644)
	if err != nil {
		return err
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(7 * 1024 * 1024 * 1024); err != nil { // 7 GB
		return err
	}

	// Find the next free loop device:
	const (
		LOOP_CTL_GET_FREE = 0x4c82
		LOOP_SET_FD       = 0x4c00
		LOOP_SET_STATUS64 = 0x4c04
	)

	loopctl, err := os.Open("/dev/loop-control")
	if err != nil {
		return err
	}
	defer loopctl.Close()
	free, _, errno := unix.Syscall(unix.SYS_IOCTL, loopctl.Fd(), LOOP_CTL_GET_FREE, 0)
	if errno != 0 {
		return errno
	}
	loopctl.Close()
	log.Printf("next free: %d", free)

	loopdev := fmt.Sprintf("/dev/loop%d", free)
	loop, err := os.OpenFile(loopdev, os.O_RDWR|unix.O_CLOEXEC, 0644)
	if err != nil {
		return err
	}
	defer loop.Close()
	// TODO: get this into x/sys/unix
	type LoopInfo64 struct {
		device         uint64
		inode          uint64
		rdevice        uint64
		offset         uint64
		sizeLimit      uint64
		number         uint32
		encryptType    uint32
		encryptKeySize uint32
		flags          uint32
		filename       [64]byte
		cryptname      [64]byte
		encryptkey     [32]byte
		init           [2]uint64
	}
	const (
		LO_FLAGS_READ_ONLY = 1
		LO_FLAGS_AUTOCLEAR = 4 // loop device will autodestruct on last close
	)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, loop.Fd(), LOOP_SET_FD, uintptr(f.Fd())); errno != 0 {
		return errno
	}
	var filename [64]byte
	copy(filename[:], []byte("root"))
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, loop.Fd(), LOOP_SET_STATUS64, uintptr(unsafe.Pointer(&LoopInfo64{
		flags:    LO_FLAGS_AUTOCLEAR | LO_FLAGS_READ_ONLY,
		filename: filename,
	}))); errno != 0 {
		return errno
	}

	sfdisk := exec.Command("sudo", "sfdisk", loopdev)
	sfdisk.Stdin = strings.NewReader(`label:gpt
size=550M, name=boot, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B
name=root`)
	sfdisk.Stdout = os.Stdout
	sfdisk.Stderr = os.Stderr
	if err := sfdisk.Run(); err != nil {
		return xerrors.Errorf("%v: %v", sfdisk.Args, err)
	}

	losetup := exec.Command("sudo", "losetup", "--show", "--find", "--partscan", p.diskImg)
	losetup.Stderr = os.Stderr
	out, err := losetup.Output()
	if err != nil {
		return xerrors.Errorf("%v: %v", losetup.Args, err)
	}

	base := strings.TrimSpace(string(out))
	log.Printf("base: %q", base)

	esp := base + "p1"
	boot := esp
	// p2 is the GRUB BIOS boot partition
	root := base + "p2"

	mkfs := exec.Command("sudo", "mkfs.fat", "-F32", esp)
	mkfs.Stdout = os.Stdout
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return xerrors.Errorf("%v: %v", mkfs.Args, err)
	}

	var luksUUID string
	if p.encrypt {
		luksFormat := exec.Command("sudo", "cryptsetup", "luksFormat", root, "-")
		luksFormat.Stdin = strings.NewReader(p.cryptPassword)
		luksFormat.Stdout = os.Stdout
		luksFormat.Stderr = os.Stderr
		if err := luksFormat.Run(); err != nil {
			return xerrors.Errorf("%v: %v", luksFormat.Args, err)
		}

		luksUUID, err = uuid(root, "fs")
		if err != nil {
			return xerrors.Errorf("lsblk: %v", err)
		}

		luksOpen := exec.Command("sudo", "cryptsetup", "open", "--type=luks", "--key-file=-", root, "cryptroot")
		luksOpen.Stdin = strings.NewReader(p.cryptPassword)
		luksOpen.Stdout = os.Stdout
		luksOpen.Stderr = os.Stderr
		if err := luksOpen.Run(); err != nil {
			return xerrors.Errorf("%v: %v", luksOpen.Args, err)
		}
		defer func() {
			luksClose := exec.Command("sudo", "cryptsetup", "close", "cryptroot")
			luksClose.Stdout = os.Stdout
			luksClose.Stderr = os.Stderr
			if err := luksClose.Run(); err != nil {
				log.Printf("%v: %v", luksClose.Args, err)
			}
		}()

		root = "/dev/mapper/cryptroot"
	}

	//make root partition
	mkfs = exec.Command("sudo", "mkfs.btrfs", root)
	mkfs.Stdout = os.Stdout
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return xerrors.Errorf("%v: %v", mkfs.Args, err)
	}

	//mount root and create default subvolumes
	if err := os.MkdirAll("/mnt", 0755); err != nil {
		return err
	}
	if err := syscall.Mount(root, "/mnt", "btrfs", syscall.MS_MGC_VAL|0, ""); err != nil {
		return xerrors.Errorf("mount %s %s: %v", root, "/mnt", err)
	}
	for _, entry := range []struct {
		path          string
		defaultSubvol bool
	}{
		{"sysroot", true},
		//{"etc", false}, //defer creation to after systemd-firstboot
		{"var", false},
		{"roimg", false},
		{"home", false},
	} {
		createSubvolume(filepath.Join("/mnt", entry.path))
		if entry.defaultSubvol {
			subvol := exec.Command("sudo", "btrfs", "subvolume", "set-default", filepath.Join("/mnt", entry.path))
			subvol.Stdout = os.Stdout
			subvol.Stderr = os.Stderr
			if err := subvol.Run(); err != nil {
				return xerrors.Errorf("Subvolume create %v: %v", subvol.Args, err)
			}
		}
	}
	if err := syscall.Unmount("/mnt", 0); err != nil {
		return xerrors.Errorf("unmount %s: %v", "/mnt", err)
	}

	//mounts
	for _, entry := range []struct {
		dest, src, fs string
		extraflags    uintptr
		options       string
	}{
		{"/mnt", root, "btrfs", 0, "subvol=/sysroot"},
		//{"/mnt/etc", root, "btrfs", 0, "subvol=/etc"}, //defer to after systemd-firstboot
		{"/mnt/var", root, "btrfs", 0, "subvol=/var"},
		{"/mnt/home", root, "btrfs", 0, "subvol=/home"},
		{"/mnt/roimg", root, "btrfs", 0, "subvol=/roimg"},
		{"/mnt/boot", boot, "vfat", 0, ""},
		//{"/mnt/boot/efi", esp, "vfat", 0, ""},
		{"/mnt/dev", "/dev", "", syscall.MS_BIND, ""},
		{"/mnt/sys", "/sys", "", syscall.MS_BIND, ""},
	} {
		if err := os.MkdirAll(entry.dest, 0755); err != nil {
			return err
		}
		if err := syscall.Mount(entry.src, entry.dest, entry.fs, syscall.MS_MGC_VAL|entry.extraflags, entry.options); err != nil {
			return xerrors.Errorf("mount %s %s: %v", entry.src, entry.dest, err)
		}
		defer syscall.Unmount(entry.dest, 0)
	}

	if err := p.pack("/mnt"); err != nil {
		return err
	}

	// ls := exec.Command("sudo", "ls", "/mnt/proc")
	// ls.Stderr = os.Stderr
	// ls.Stdout = os.Stdout
	// if err := ls.Run(); err != nil {
	// 	return xerrors.Errorf("%v: %v", ls.Args, err)
	// }

	//remove and recreate /mnt/proc to allow mounting real /proc
	os.RemoveAll("/mnt/proc")
	if err := os.MkdirAll("/mnt/proc", 0755); err != nil {
		return err
	}
	if err := syscall.Mount("/proc", "/mnt/proc", "", syscall.MS_MGC_VAL|syscall.MS_BIND, ""); err != nil {
		return xerrors.Errorf("mount %s %s: %v", "/proc", "/mnt/proc", err)
	}
	defer syscall.Unmount("/mnt/proc", 0)

	chown := exec.Command("sudo", "chown", os.Getenv("USER"), "/mnt/ro")
	chown.Stderr = os.Stderr
	chown.Stdout = os.Stdout
	if err := chown.Run(); err != nil {
		return xerrors.Errorf("%v: %v", chown.Args, err)
	}
	join, err := cmdfuse.Mount([]string{"-repo=/mnt/roimg", "/mnt/ro"})
	if err != nil {
		return err
	}
	defer fuse.Unmount("/mnt/ro")

	//if err := os.MkdirAll("/mnt/boot/grub", 0755); err != nil {
	//	return err
	//}

	if p.encrypt {
		crypttab := fmt.Sprintf("cryptroot UUID=%s none luks,discard\n", luksUUID)
		if err := ioutil.WriteFile("/mnt/etc/crypttab", []byte(crypttab), 0644); err != nil {
			return err
		}
	}

	//get root and boot uuid
	rootUUID, err := uuid(root, "fs")
	if err != nil {
		return xerrors.Errorf(`uuid(root=%v, "fs"): %v`, root, err)
	}
	bootUUID, err := uuid(boot, "fs")
	if err != nil {
		return xerrors.Errorf(`uuid(boot=%v, "fs"): %v`, boot, err)
	}

	{
		fstab := ""
		if p.encrypt {
			fstab = "/dev/mapper/cryptroot / btrfs defaults,x-systemd.device-timeout=0 1 1\n"
		} /*else {
			fstab = "UUID=" + rootUUID + " / btrfs defaults 0 1\n"
		}*/
		fstab = fstab + "UUID=" + bootUUID + " /boot vfat defaults 0 1\n"
		/*espUUID, err := uuid(esp, "part")
		if err != nil {
			return xerrors.Errorf(`uuid(esp=%v, "part"): %v`, esp, err)
		}
		fstab = fstab + "UUID=" + espUUID + " /boot/efi vfat defaults 0 1\n"*/
		if err := ioutil.WriteFile("/mnt/etc/fstab", []byte(fstab), 0644); err != nil {
			return err
		}
	}

	{
		shells := strings.Join([]string{
			"/bin/zsh",
			"/bin/bash",
			"/bin/sh",
		}, "\n") + "\n"
		if err := ioutil.WriteFile("/mnt/etc/shells", []byte(shells), 0644); err != nil {
			return err
		}
	}

	if err := ioutil.WriteFile("/mnt/etc/dracut.conf.d/kbddir.conf", []byte("kbddir=/ro/share\n"), 0644); err != nil {
		return err
	}
	dracut := exec.Command("sudo", "chroot", "/mnt", "sh", "-c", "dracut --add-drivers btrfs /boot/initramfs-5.1.9-9.img 5.1.9")
	dracut.Stderr = os.Stderr
	dracut.Stdout = os.Stdout
	if err := dracut.Run(); err != nil {
		return xerrors.Errorf("%v: %v", dracut.Args, err)
	}

	var params []string
	params = append(params, "root=UUID="+rootUUID)
	if !p.serialOnly {
		params = append(params, "console=tty1")
	}
	if p.encrypt {
		params = append(params, "rd.luks=1 rd.luks.uuid="+luksUUID+" rd.luks.name="+luksUUID+"=cryptroot")
	}
	if p.bootDebug {
		params = append(params, "systemd.log_level=debug systemd.log_target=console")
	}

	/*
		log.Println("Installing grub...")
		install := exec.Command("sudo", "chroot", "/mnt", "/ro/grub2-amd64-2.02-3/bin/grub-install", "--target=i386-pc", base)
		install.Stderr = os.Stderr
		install.Stdout = os.Stdout
		if err := install.Run(); err != nil {
			return xerrors.Errorf("%v: %v", install.Args, err)
		}

		install = exec.Command("sudo", "chroot", "/mnt", "/ro/grub2-efi-amd64-2.02-3/bin/grub-install", "--target=x86_64-efi", "--efi-directory=/boot/efi", "--removable", "--no-nvram", "--boot-directory=/boot")
		install.Stderr = os.Stderr
		install.Stdout = os.Stdout
		if err := install.Run(); err != nil {
			return xerrors.Errorf("%v: %v", install.Args, err)
		}

		log.Println("Configuring grub...")
		mkconfigCmd := "GRUB_DISABLE_LINUX_UUID=true GRUB_DISABLE_LINUX_PARTUUID=true GRUB_CMDLINE_LINUX=\"console=ttyS0,115200 " + strings.Join(params, " ") + " init=/init systemd.setenv=PATH=/bin rw\" GRUB_TERMINAL=serial grub-mkconfig -o /boot/grub/grub.cfg"
		mkconfig := exec.Command("sudo", "chroot", "/mnt", "sh", "-c", mkconfigCmd)
		mkconfig.Stderr = os.Stderr
		mkconfig.Stdout = os.Stdout
		if err := mkconfig.Run(); err != nil {
			return xerrors.Errorf("%v: %v", mkconfig.Args, err)
		}

		if err := ioutil.WriteFile("/mnt/etc/update-grub", []byte("#!/bin/sh\n"+mkconfigCmd+"\n"), 0755); err != nil {
			return xerrors.Errorf("writing /etc/update-grub: %v", err)
		}
	*/
	log.Println("Installing bootloader")
	install := exec.Command("sudo", "chroot", "/mnt", "/ro/systemd-amd64-239-10/bin/bootctl" /*"--path=/boot", */, "--no-variables", "install")
	install.Stderr = os.Stderr
	install.Stdout = os.Stdout
	if err := install.Run(); err != nil {
		return xerrors.Errorf("%v: %v", install.Args, err)
	}

	if err := fuse.Unmount("/mnt/ro"); err != nil {
		return xerrors.Errorf("unmount /mnt/ro: %v", err)
	}

	if err := join(context.Background()); err != nil {
		return xerrors.Errorf("fuse: %v", err)
	}

	chown = exec.Command("sudo", "chown", "root", "/mnt/ro")
	chown.Stderr = os.Stderr
	chown.Stdout = os.Stdout
	if err := chown.Run(); err != nil {
		return xerrors.Errorf("%v: %v", chown.Args, err)
	}

	// create /etcb subvolume and move /etc contents to it
	log.Printf("Fixing /etc")

	os.MkdirAll("/mnt/tmp/btrfsroot", 0700)
	if err := syscall.Mount(root, "/mnt/tmp/btrfsroot", "btrfs", syscall.MS_MGC_VAL, "subvol=/"); err != nil {
		return err
	}
	createSubvolume("/mnt/tmp/btrfsroot/etcb")

	os.MkdirAll("/mnt/etcb", 0755)
	if err := syscall.Mount(root, "/mnt/etcb", "btrfs", syscall.MS_MGC_VAL, "subvol=/etcb"); err != nil {
		return xerrors.Errorf("mount %s %s: %v", root, "/mnt/etcb", err)
	}
	if err := copyDir("/mnt/etc", "/mnt/etcb/etc"); err != nil {
		return err
	}
	if err := os.RemoveAll("/mnt/etc"); err != nil {
		return err
	}

	log.Println("Creating default subvolumes")
	createSubvolume("/mnt/tmp/btrfsroot/snapshots")
	os.MkdirAll("/mnt/tmp/btrfsroot/snapshots/default", 0700)
	os.MkdirAll("/mnt/tmp/btrfsroot/snapshots/pristine", 0700)

	//TODO allow read-only snapshots (make read-only only upper dir)
	if err := createBtrfsSnapshot("/mnt/etcb", "/mnt/tmp/btrfsroot/snapshots/pristine/etcb", false); err != nil {
		return xerrors.Errorf("create snaphot: %v", err)
	}
	if err := createBtrfsSnapshot("/mnt/roimg", "/mnt/tmp/btrfsroot/snapshots/pristine/roimg", true); err != nil {
		return xerrors.Errorf("create snaphot: %v", err)
	}
	if err := createBtrfsSnapshot("/mnt/etcb", "/mnt/tmp/btrfsroot/snapshots/default/etcb", false); err != nil {
		return xerrors.Errorf("create snaphot: %v", err)
	}
	if err := createBtrfsSnapshot("/mnt/roimg", "/mnt/tmp/btrfsroot/snapshots/default/roimg", false); err != nil {
		return xerrors.Errorf("create snaphot: %v", err)
	}

	log.Println("Creating boot configurations")
	if err := ioutil.WriteFile("/mnt/boot/loader/loader.conf", []byte(`
timeout 4
console-mode keep
default  default
console-mode max
editor   yes
#auto-firmware 1

`), 0644); err != nil {
		return err
	}
	if err := ioutil.WriteFile("/mnt/boot/loader/entries/default.conf", []byte(`
title   Default snapshot
linux   /vmlinuz-5.1.9-9
initrd  /initramfs-5.1.9-9.img
options  console=ttyS0,115200 ro rootflags=subvol=sysroot  root=UUID=`+rootUUID+` init=/init snapshot=default systemd.setenv=PATH=/bin rw
`), 0644); err != nil {
		return err
	}
	if err := ioutil.WriteFile("/mnt/boot/loader/entries/pristine.conf", []byte(`
title   pristine
linux   /vmlinuz-5.1.9-9
initrd  /initramfs-5.1.9-9.img
options  console=ttyS0,115200 ro rootflags=subvol=sysroot  root=UUID=`+rootUUID+` init=/init snapshot=pristine systemd.setenv=PATH=/bin rw
`), 0644); err != nil {
		return err
	}

	if err := syscall.Unmount("/mnt/tmp/btrfsroot", 0); err != nil {
		return err
	}

	syscall.Unmount("/mnt/etcb", 0)
	os.RemoveAll("/mnt/etcb")

	//unmount /mnt and everything mounted below
	for _, m := range []string{"sys", "dev", "proc", "boot", "home", "var", "roimg"} {
		if err := syscall.Unmount(filepath.Join("/mnt", m), 0); err != nil {
			return xerrors.Errorf("unmount /mnt/%s: %v", m, err)
		}
	}

	losetup = exec.Command("sudo", "losetup", "-d", base)
	losetup.Stdout = os.Stdout
	losetup.Stderr = os.Stderr
	if err := losetup.Run(); err != nil {
		return xerrors.Errorf("%v: %v", losetup.Args, err)
	}

	return nil
}

func createSubvolume(path string) error {
	subvol := exec.Command("sudo", "btrfs", "subvolume", "create", path)
	subvol.Stdout = os.Stdout
	subvol.Stderr = os.Stderr
	if err := subvol.Run(); err != nil {
		return xerrors.Errorf("Subvolume create %v: %v", subvol.Args, err)
	}
	return nil
}

// move source to dest, preserving permissions
func moveContent(source, dest string) error {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	fileInfo, err := f.Readdir(-1)
	if len(fileInfo) == 0 {
		return nil
	}
	f.Close()
	if err != nil {
		return err
	}
	for _, file := range fileInfo {
		if file.IsDir() {
			err := moveContent(filepath.Join(source, file.Name()), filepath.Join(dest, file.Name()))
			if err != nil {
				return nil
			}
			continue
		}
		destFilename := filepath.Join(dest, file.Name())
		sourceFilename := filepath.Join(source, file.Name())
		// file.Mode() does not return the correct mode. Why?
		infoFile, err := os.Stat(filepath.Join(source, file.Name()))
		if err != nil {
			return err
		}
		err = copyFile(sourceFilename, destFilename)
		if err != nil {
			return nil
		}
		err = os.Chmod(destFilename, infoFile.Mode())
		if err != nil {
			return nil
		}

		// don't care if it fails
		os.RemoveAll(sourceFilename)
	}

	return nil
}

func copyDir(source, dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return xerrors.Errorf("copyContent %q: %w", dest, err)
	}
	cmd := exec.Command("sudo", "sh", "-c", "cp -r "+source+" "+dest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return xerrors.Errorf("copyContent %v: %w", cmd.Args, err)
	}
	os.Chmod(dest, 0775)

	return nil
}

func adduser(root, line string) error {
	// TODO: pam requires an entry in /etc/shadow, too, even if the password is disabled
	f, err := os.OpenFile(filepath.Join(root, "etc", "passwd"), os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte(line + "\n")); err != nil {
		return err
	}
	return f.Close()
}

func addgroup(root, line string) error {
	f, err := os.OpenFile(filepath.Join(root, "etc", "group"), os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte(line + "\n")); err != nil {
		return err
	}
	return f.Close()
}

func uuid(blockdev, kind string) (string, error) {
	st, err := os.Stat(blockdev)
	if err != nil {
		return "", err
	}
	rdev := st.Sys().(*syscall.Stat_t).Rdev
	const (
		// hard-coded, as in systemd-241/src/libsystemd/sd-device/sd-device.c
		udevDb = "/run/udev/data/b%d:%d"
	)
	b, err := ioutil.ReadFile(fmt.Sprintf(udevDb, unix.Major(rdev), unix.Minor(rdev)))
	if err != nil {
		return "", err
	}
	prefix := "E:ID_FS_UUID_ENC=" // kind == fs
	if kind == "part" {
		prefix = "E:ID_PART_ENTRY_UUID="
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return strings.TrimPrefix(line, prefix), nil
	}
	return "", nil
}
