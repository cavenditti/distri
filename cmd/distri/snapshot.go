package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	_ "github.com/distr1/distri/internal/oninterrupt"
	"golang.org/x/xerrors"

	_ "net/http/pprof"
)

type systemconfig struct {
	snapshot string
	rootDev  string
}

const snapshotsroot = "/tmp/snapshotsroot"

func getSystemconfig() (systemconfig, error) {
	//read snapshot and root device from kernel cmdline
	var res systemconfig
	params := []string{"snapshot", "root=UUID", "root=PARTUUID", "root"}
	m, err := parseCmdline(params)
	if err != nil {
		return res, err
	}

	// get snapshot
	snapshot, ok := m["snapshot"]
	if ok {
		log.Printf("system snapshot: " + snapshot)
	} else {
		log.Printf("no snapshot defined, using default.")
		snapshot = "default"
	}

	// get root
	var rootDev string
	if rd, ok := m["root=UUID"]; ok {
		rootDev = "/dev/disk/by-uuid/" + rd
	} else if rd, ok := m["root=PARTUUID"]; ok {
		rootDev = "/dev/disk/by-partuuid/" + rd
	} else if rd, ok := m["root="]; ok {
		rootDev = rd
	} else {
		return res, xerrors.Errorf("cannot read root partition from cmdline")
	}

	res.snapshot = snapshot
	res.rootDev = rootDev

	return res, nil
}

func createBtrfsSnapshot(subvol, path string, readOnly bool) error {
	var cmd *exec.Cmd
	if readOnly {
		// FIXME needs sudo during pack but shouldn't be so
		cmd = exec.Command("sudo", "btrfs", "subvolume", "snapshot", "-r", subvol, path)
	} else {
		cmd = exec.Command("sudo", "btrfs", "subvolume", "snapshot", subvol, path)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return err
}

func createSnapshot(args []string) error {
	fset := flag.NewFlagSet("create", flag.ExitOnError)
	var (
		// update = fset.Bool("update", false, "update existing snapshot")

		readOnly = fset.Bool("read-only", false, "create a read only snapshot")
	)
	fset.Usage = func() {
		fmt.Fprintln(os.Stderr, `distri snapshot create [options] <name>
Create a system snapshot.
			`)
		fmt.Fprintf(os.Stderr, "Flags for distri create snapshot:\n")
		fset.PrintDefaults()
	}

	fset.Parse(args)
	if fset.NArg() < 1 {
		return xerrors.Errorf("syntax: snapshot create [options] <name>")
	}

	var index int
	if len(args) > 1 {
		index = fset.NArg()
	} else {
		index = 0
	}
	name := args[index]

	config, err := getSystemconfig()
	if err != nil {
		return err
	}

	// check if snapshotsroot exists and if so return without doing anything
	_, err = os.Stat(snapshotsroot)
	if err == nil || !os.IsNotExist(err) {
		return xerrors.Errorf(snapshotsroot + " already exists")
	}

	os.MkdirAll(snapshotsroot, 0700)
	if err := syscall.Mount(config.rootDev, snapshotsroot, "btrfs", syscall.MS_MGC_VAL, "subvol=/snapshots"); err != nil {
		return xerrors.Errorf("create snapshot: %v ", err)
	}

	os.MkdirAll(filepath.Join(snapshotsroot, name), 0700)

	if *readOnly {

		for _, s := range []string{"etcb", "roimg"} {
			if err := createBtrfsSnapshot(filepath.Join(snapshotsroot, config.snapshot, s), filepath.Join(snapshotsroot, name, s), true); err != nil {
				return xerrors.Errorf("create snapshot: %v ", err)
			}
		}

	} else {

		for _, s := range []string{"etcb", "roimg"} {
			if err := createBtrfsSnapshot(filepath.Join(snapshotsroot, config.snapshot, s), filepath.Join(snapshotsroot, name, s), false); err != nil {
				return xerrors.Errorf("create snapshot: %v", err)
			}
		}
	}

	//cmd := exec.Command("blkid", "-ovalue", "-sUUID", config.rootDev)
	cmd := exec.Command("findmnt", "-noUUID", "/")
	rootUUIDb, err := cmd.Output()
	if err != nil {
		return xerrors.Errorf("cannot get root UUID")
	}
	rootUUID := string(rootUUIDb)
	fmt.Println("using root UUID: " + rootUUID)

	cmd = exec.Command("findmnt", "-noUUID", "/boot")
	bootUUIDb, err := cmd.Output()
	if err != nil {
		return xerrors.Errorf("cannot get boot UUID")
	}
	bootUUID := string(bootUUIDb)
	fmt.Println("using boot UUID: " + bootUUID)

	f, err := os.OpenFile("/etc/grub.d/40_custom",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Println(err)
	}
	defer f.Close()
	if _, err := f.WriteString(`menuentry 'Snapshot ` + name + ` GNU/Linux, with Linux 5.1.9-9' {
	load_video
	insmod gzio
	insmod part_gpt
	insmod ext2
	  search --no-floppy --fs-uuid --set=root  ` + bootUUID + `
	echo    'Loading Snapshot ` + name + ` 5.1.9-9 ...'
	linux   /vmlinuz-5.1.9-9 console=ttyS0,115200 ro rootflags=subvol=sysroot  root=UUID=` + rootUUID + ` init=/init snapshot=` + name + ` systemd.setenv=PATH=/bin rw
	initrd  /initramfs-5.1.9-9.img
}
`); err != nil {
		log.Println(err)
	}

	syscall.Unmount(snapshotsroot, 0)
	os.RemoveAll(snapshotsroot)

	return nil
}

func listSnapshots(args []string) error {
	config, err := getSystemconfig()
	if err != nil {
		return err
	}

	os.MkdirAll(snapshotsroot, 0700)
	if err := syscall.Mount(config.rootDev, "/tmp/btrfsroot", "btrfs", syscall.MS_MGC_VAL, "subvol=/snapshots"); err != nil {
		return err
	}

	f, err := os.Open(snapshotsroot)
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
			fmt.Println(file.Name())
		}
	}

	syscall.Unmount(snapshotsroot, 0)
	os.RemoveAll(snapshotsroot)

	return nil
}

func snapshot(arg []string) error {
	type cmd struct {
		fn func(args []string) error
	}
	verbs := map[string]cmd{
		"list":   {listSnapshots},
		"create": {createSnapshot},
	}

	args := flag.Args()
	verb := "list"
	if len(args) > 1 {
		verb, args = args[1], args[2:]
	}

	if verb == "help" {
		if len(args) != 1 {
			fmt.Fprintf(os.Stderr, "distri snapshot <command> [-flags] <args>\n")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "Snapshots commands:\n")
			fmt.Fprintf(os.Stderr, "\tlist  - list snapshots\n")
			fmt.Fprintf(os.Stderr, "\tcreate   - create new snapshot from current configuration\n")
			os.Exit(2)
		}
		verb = args[0]
		args = []string{"-help"}
	}
	v, ok := verbs[verb]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown snapshot command %q\n", verb)
		fmt.Fprintf(os.Stderr, "syntax: distri snapshot <command> [options]\n")
		os.Exit(2)
	}
	if err := v.fn(args); err != nil {
		if *debug {
			fmt.Fprintf(os.Stderr, "%s: %+v\n", verb, err)
		} else {
			fmt.Fprintf(os.Stderr, "%s: %v\n", verb, err)
		}
		os.Exit(1)
	}

	return nil
}
