package main

import (
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func parseCmdline(arg []string) (map[string]string, error) {
	bcmd, err := ioutil.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, err
	}

	cmdlineString := string(bcmd[:])
	cmdline := strings.Split(cmdlineString, " ")

	var m map[string]string
	m = make(map[string]string)

	for _, param := range cmdline {
		for _, k := range arg {
			if strings.HasPrefix(param, k) {
				param = strings.Replace(param, k, "", 1)
				m[k] = strings.Replace(param, "=", "", 1)
			}
		}
	}
	return m, err
}

func createEtcOverlay() error {
	syscall.Mount("overlay", "/etc", "overlay", syscall.MS_MGC_VAL, "lowerdir=/ro/etc,upperdir=/run/etc/u,work=/run/etc/w")

	return nil
}

func bootfuse() error {
	// TODO: start fuse in separate process, make argv[0] be '@' as per
	// https://www.freedesktop.org/wiki/Software/systemd/RootStorageDaemons/

	r, w, err := os.Pipe() // for readiness notification
	if err != nil {
		return err
	}

	fuse := exec.Command("/init", "fuse", "-repo=/roimg", "-readiness=3", "/ro")
	fuse.ExtraFiles = []*os.File{w}
	fuse.Env = []string{
		// Set TZ= so that the time package does not try to open /etc/localtime,
		// which is a symlink into /ro, which would deadlock when called from
		// the FUSE request handler.
		"TZ=",
		"TMPDIR=/ro-tmp",
	}
	fuse.Stderr = os.Stderr
	fuse.Stdout = os.Stdout
	if err := fuse.Start(); err != nil {
		return err
	}

	// Close the write end of the pipe in the parent process.
	if err := w.Close(); err != nil {
		return err
	}

	// Wait until the read end of the pipe returns EOF
	if _, err := ioutil.ReadAll(r); err != nil {
		return err
	}

	return nil
}

func pid1() error {
	log.SetPrefix("distrib -> ")

	config, err := getSystemconfig()
	if err != nil {
		return err
	}

	// mount /roimg
	log.Printf("mounting /roimg snapshot")
	if err := syscall.Mount(config.rootDev, "/roimg", "btrfs", syscall.MS_MGC_VAL, "subvol=/snapshots/"+config.snapshot+"/roimg"); err != nil {
		// if failed, try mounting /roimg which subvolume should always exists
		log.Printf("!! failed mounting subvolume: /snapshots/" + config.snapshot + "/roimg !!\ttrying /roimg instead")
		if err := syscall.Mount(config.rootDev, "/roimg", "btrfs", syscall.MS_MGC_VAL, "subvol=/roimg"); err != nil {
			return err
		}
	}

	// mount packages
	log.Printf("FUSE-mounting package store /roimg on /ro")
	if err := bootfuse(); err != nil {
		return err
	}

	// mount /etc
	log.Printf("mounting /etc snapshot and overlay")
	if err := os.MkdirAll("/run/etcb", 0755); err != nil {
		return err
	}
	if err := syscall.Mount(config.rootDev, "/run/etcb", "btrfs", syscall.MS_MGC_VAL, "subvol=/snapshots/"+config.snapshot+"/etcb"); err != nil {
		// if failed, try mounting /etcb subvolume which should always exists
		log.Printf("!! failed mounting subvolume: /snapshots/" + config.snapshot + "/etcb !!\ttrying /etcb instead")
		if err := syscall.Mount(config.rootDev, "/run/etcb", "btrfs", syscall.MS_MGC_VAL, "subvol=/etcb"); err != nil {
			return err
		}
	}
	if err := os.MkdirAll("/run/etcb/.workdir", 0700); err != nil {
		return err
	}
	if err := os.MkdirAll("/etc", 0755); err != nil {
		return err
	}
	if err := syscall.Mount("overlay", "/etc", "overlay", syscall.MS_MGC_VAL, "lowerdir=/ro/etc,upperdir=/run/etcb/etc,workdir=/run/etcb/.workdir"); err != nil {
		return err
	}

	// start systemd
	log.Printf("starting systemd")
	// TODO: readdir /ro (does not mount any images)
	// TODO: keep most recent systemd entry
	const systemd = "/ro/systemd-amd64-239-10/out/lib/systemd/systemd" // TODO(later): glob?
	return syscall.Exec(systemd, []string{systemd}, nil)
}
