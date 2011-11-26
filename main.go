package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

var kernelArgs = flag.String("kernel-args", "root=/dev/sda1 ro",
	"Commandline flags to pass to the kernel.")

var diskSize = flag.Uint64("disk-size", 128,
	"Size of the created disk image in MB")

var printLog = flag.Bool("print-log", false,
	"Print the stdout/err log of commands that were run")

var Usage = func() {
	fmt.Fprintf(os.Stderr, `Usage: %s outfile kernel source...

Multiple sources can be provided. If a source is a tarball, it is
extracted to the root of the filesystem. If it's a directory, it
is copied verbatim to the root of the filesystem.

`, os.Args[0])
	flag.PrintDefaults()
}

type LoggingExec struct {
	Stdout, Stderr bytes.Buffer
}

func (l *LoggingExec) Cmd(cmd string, args ...string) *exec.Cmd {
	header := fmt.Sprintf("\n=== %s %s\n", cmd, args)
	l.Stdout.WriteString(header)
	l.Stderr.WriteString(header)
	c := exec.Command(cmd, args...)
	c.Stdout = &l.Stdout
	c.Stderr = &l.Stderr
	return c
}

func (l *LoggingExec) PrintLog() {
	if l.Stdout.Len() > 0 {
		fmt.Fprintf(os.Stderr, `
=====================================================
================= stdout log ========================
=====================================================
`)
		l.Stdout.WriteTo(os.Stderr)
	}
	if l.Stderr.Len() > 0 {
		fmt.Fprintf(os.Stderr, `
=====================================================
================= stderr log ========================
=====================================================
`)
		l.Stderr.WriteTo(os.Stderr)
	}
}

var exe LoggingExec

// We use panic instead of a direct print+os.Exit so that goroutines
// can unwind their deferred calls. This is because we use defers to
// undo some fairly hairy state changes (e.g. loopback device
// mounting), and don't want to just leave it in place when we error.
func Exit(err interface{}) {
	panic(err)
}

func Log(entry string) {
	fmt.Fprintln(os.Stderr, entry)
}

func CheckPrograms(programs ...string) {
	missing := false
	for _, program := range programs {
		if _, err := exec.LookPath(program); err != nil {
			fmt.Fprintln(os.Stderr, "Couldn't find program:", program)
			missing = true
		}
	}
	if missing {
		Exit("Some required programs are missing")
	}
}

const syslinuxConfig = `
PROMPT 0
DEFAULT linux
LABEL linux
    LINUX %s
    APPEND %s
`

func main() {
	flag.Parse()
	if flag.NArg() < 3 {
		Usage()
		return
	}

	if os.Getuid() != 0 {
		Log("Warning: not running as root, image construction will likely fail.")
		Log("Continuing anyway, in case you have root-equivalent capabilities set.")
	}

	defer func() {
		if err := recover(); err != nil {
			exe.PrintLog()
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		} else if *printLog {
			exe.PrintLog()
		}
	}()

	CheckPrograms(
		"dd",
		"kpartx",
		"losetup",
		"mkfs.ext3",
		"mount",
		"sfdisk",
		"tar",
		"umount",
		"rsync",
		"extlinux")

	outfile := flag.Arg(0)
	kernel := flag.Arg(1)
	sources := flag.Args()[2:]

	Log("Creating filesystem image")
	err := exe.Cmd("dd",
		"if=/dev/zero",
		fmt.Sprintf("of=%s", flag.Arg(0)),
		"bs=1M",
		fmt.Sprintf("count=%d", *diskSize)).Run()
	if err != nil {
		Exit(err)
	}

	Log("Creating partition table")
	cmd := exe.Cmd("sfdisk", outfile)
	cmd.Stdin = bytes.NewBufferString(";;;*;\n")
	if err = cmd.Run(); err != nil {
		Exit(err)
	}

	Log("Setting up loop device")
	cmd = exe.Cmd("losetup", "--show", "-f", outfile)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err = cmd.Run(); err != nil {
		Exit(err)
	}
	device := strings.Trim(buf.String(), "\n")
	defer func() {
		Log("Tearing down loop device")
		exe.Cmd("losetup", "-d", device).Run()
	}()

	Log("Writing syslinux MBR")
	cmd = exe.Cmd("dd",
		"if=/usr/lib/extlinux/mbr.bin",
		fmt.Sprintf("of=%s", device),
		"bs=440",
		"count=1")
	if err = cmd.Run(); err != nil {
		Exit(err)
	}

	Log("Setting up partition loop device")
	if err = exe.Cmd("kpartx", "-a", "-v", device).Run(); err != nil {
		Exit(err)
	}
	defer func() {
		Log("Tearing down partition loop device")
		exe.Cmd("kpartx", "-d", device).Run()
	}()

	partition := fmt.Sprintf("/dev/mapper/%sp1", path.Base(device))
	Log("Creating filesystem")
	if err = exe.Cmd("mkfs.ext3", partition).Run(); err != nil {
		Exit(err)
	}

	mountpoint, err := ioutil.TempDir("", "mksysimage")
	if err != nil {
		Exit(err)
	}
	mountpoint, err = filepath.Abs(mountpoint)
	if err != nil {
		Exit(err)
	}
	defer os.Remove(mountpoint)

	Log("Mounting the partition")
	if err = exe.Cmd("mount", "-o", "loop", "-t", "ext3", partition, mountpoint).Run(); err != nil {
		Exit(err)
	}
	defer func() {
		Log("Unmounting the partition")
		exe.Cmd("umount", "-l", mountpoint).Run()
	}()

	Log("Installing extlinux")
	extlinux := path.Join(mountpoint, "boot")
	if err = os.MkdirAll(extlinux, 0700); err != nil {
		Exit(err)
	}
	if err = exe.Cmd("cp", kernel, extlinux).Run(); err != nil {
		Exit(err)
	}
	cfgfile, err := os.Create(path.Join(extlinux, "syslinux.cfg"))
	if err != nil {
		Exit(err)
	}
	cfg := fmt.Sprintf(syslinuxConfig, path.Base(kernel), *kernelArgs)
	if _, err = cfgfile.Write([]byte(cfg)); err != nil {
		Exit(err)
	}
	cfgfile.Close()
	if err := exe.Cmd("extlinux", "--install", mountpoint).Run(); err != nil {
		Exit(err)
	}

	for _, source := range sources {
		Log(fmt.Sprintf("Populating filesystem from %s", source))
		source, err = filepath.Abs(source)
		if err != nil {
			Exit(err)
		}
		st, err := os.Stat(source)
		if err != nil {
			Exit(err)
		}
		var cmd *exec.Cmd
		if st.IsDirectory() {
			cmd = exe.Cmd("rsync", "-RrvP", ".", mountpoint)
			cmd.Dir = source
			if err != nil {
				Exit(err)
			}
		} else {
			cmd = exe.Cmd("tar", "xvf", source)
			if err != nil {
				Exit(err)
			}
			cmd.Dir = mountpoint
		}
		if err = cmd.Run(); err != nil {
			Exit(err)
		}
	}

	Log("Build complete, cleaning up")
}