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
	"errors"
)

var kernelArgs = flag.String("kernel-args", "root=/dev/sda1 ro",
	"Commandline flags to pass to the kernel")

var initrd = flag.String("kernel-initrd", "",
	"Initrd file to give the kernel on bootup, if any")

var diskSize = flag.Uint64("disk-size", 128,
	"Size of the created disk image in MB")

var printLog = flag.Bool("print-log", false,
	"Print the stdout/err log of commands that were run")

var printFs = flag.Bool("print-fs", false,
	"Print the FS image tree to stdout on completion")

var format = flag.String("format", "raw",
	"Format of the disk image (raw, vdi, vmdk, vhd)")

var vboxUuid = flag.String("vbox-uuid", "",
	"If outputting to VDI, the UUID of the disk")

var Usage = func() {
	fmt.Fprintf(os.Stderr, `Usage: %s outfile kernel root:source...

Multiple sources can be provided. If a source is a tarball, it is
extracted to the root of the filesystem. If it's a directory, it is
copied verbatim to the root of the filesystem. Each source is
overlayed in the FS image at its corresponding root.

Example:
  sudo mksysimage out.raw vmlinuz /:./system/ /etc:conf.tgz

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
		Log(fmt.Sprintf("Checking for program %s", program))
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
    %s
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

	outfinal := flag.Arg(0)
	outfile := fmt.Sprintf("%s.tmp", outfinal)
	kernel := flag.Arg(1)
	sources := flag.Args()[2:]

	if _, err := os.Stat(outfinal); err == nil {
		Exit("Output file already exists")
	}

	programs := []string{
		"dd",
		"kpartx",
		"losetup",
		"mkfs.ext3",
		"mount",
		"sfdisk",
		"tar",
		"umount",
		"rsync",
		"extlinux",
	}

	switch *format {
	case "raw":
	case "vdi", "vmdk", "vhd":
		programs = append(programs, "vboxmanage")
	default:
		Exit(fmt.Sprintf("Unknown format %s", *format))
	}

	CheckPrograms(programs...)

	Log("Creating filesystem image")
	err := exe.Cmd("dd",
		"if=/dev/zero",
		fmt.Sprintf("of=%s", outfile),
		"bs=1M",
		fmt.Sprintf("count=%d", *diskSize)).Run()
	if err != nil {
		Exit(err)
	}
	defer func() {
		exe.Cmd("rm", "-f", outfile).Run()
	}()
	if *format == "vdi" && *vboxUuid != "" {
		defer func() {
			Log("Setting disk UUID")
			if err = exe.Cmd("vboxmanage", "internalcommands", "sethduuid", outfinal, *vboxUuid).Run(); err != nil {
				Exit(err)
			}
		}()
	}
	defer func() {
		if *format == "raw" {
			if err = exe.Cmd("mv", "-f", outfile, outfinal).Run(); err != nil {
				Exit(err)
			}
		} else {
			Log(fmt.Sprintf("Creating %s image", *format))
			cmd := exe.Cmd("vboxmanage", "convertfromraw",
				outfile, outfinal,
				fmt.Sprintf("--format=%s", strings.ToUpper(*format)))
			if err = cmd.Run(); err != nil {
				Exit(err)
			}
		}
	}()

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
	var initrdcfg string
	if *initrd != "" {
		if err = exe.Cmd("cp", *initrd, extlinux).Run(); err != nil {
			Exit(err)
		}
		initrdcfg = fmt.Sprintf("INITRD %s", path.Base(*initrd))
	}
	cfgfile, err := os.Create(path.Join(extlinux, "syslinux.cfg"))
	if err != nil {
		Exit(err)
	}
	cfg := fmt.Sprintf(syslinuxConfig,
		path.Base(kernel), *kernelArgs, initrdcfg)
	if _, err = cfgfile.Write([]byte(cfg)); err != nil {
		Exit(err)
	}
	cfgfile.Close()
	if err := exe.Cmd("extlinux", "--install", extlinux).Run(); err != nil {
		Exit(err)
	}

	for _, rootandsource := range sources {
		parts := strings.SplitN(rootandsource, ":", 2)
		if len(parts) != 2 {
			Exit(errors.New(fmt.Sprintf("Malformed source %s", rootandsource)))
		}

		root := parts[0]
		source := parts[1]

		Log(fmt.Sprintf("Populating %s from %s", root, source))

		if !filepath.IsAbs(root) {
			Exit("Given source root isn't absolute")
		}
		root = filepath.Join(mountpoint, root)
		if err = os.MkdirAll(root, 0700); err != nil {
			Exit(err)
		}

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
			cmd = exe.Cmd("rsync", "-RrvP", ".", root)
			cmd.Dir = source
			if err != nil {
				Exit(err)
			}
		} else {
			cmd = exe.Cmd("tar", "xvf", source)
			if err != nil {
				Exit(err)
			}
			cmd.Dir = root
		}
		if err = cmd.Run(); err != nil {
			Exit(err)
		}
	}

	if *printFs {
		cmd = exe.Cmd("find", ".")
		cmd.Dir = mountpoint
		cmd.Stdout = os.Stdout
		if err = cmd.Run(); err != nil {
			Exit(err)
		}
	}

	Log("Build complete, cleaning up")
}
