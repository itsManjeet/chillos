package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"chillos/pkg/ensure"
)

const (
	KERNEL_VERSION = "6.15.4"
)

var (
	projectPath string
	cachePath   string
	devicePath  string

	runTest bool
	cpu     int
	memory  int
	vnc     int
	debug   bool
	clean   bool

	kernelVersion string

	deviceCachePath string
	toolchainPath   string
	imagesPath      string
	sourcesPath     string
	buildPath       string
	sysrootPath     string

	systemPath    string
	initramfsPath string
	kernelPath    string

	device Device
)

func init() {
	cur, _ := os.Getwd()
	flag.StringVar(&projectPath, "project-path", cur, "Project path")
	flag.StringVar(&cachePath, "cache-path", "", "Cache path")
	flag.StringVar(&devicePath, "device", "", "Device path")
	flag.BoolVar(&clean, "clean", false, "Clean build targets")
	flag.BoolVar(&runTest, "test", false, "run test")
	flag.IntVar(&cpu, "cpu", 1, "number of CPU for enumlation")
	flag.IntVar(&memory, "memory", 512, "memory allocated for emulation (in MBs)")
	flag.IntVar(&vnc, "vnc", -1, "VNC port")
	flag.BoolVar(&debug, "debug", false, "Wait for debugger to connect")
	flag.StringVar(&kernelVersion, "kernel", KERNEL_VERSION, "Specify kernel version")

}

func main() {
	flag.Parse()
	ensure.Success(checkup(), "failed to find required tools and libraries")
	ensure.Success(prepareDirectories(), "failed to prepare directories")

	for key, value := range device.Environ {
		os.Setenv(key, value)
	}

	os.Setenv("PATH", filepath.Join(toolchainPath, "bin")+":"+os.Getenv("PATH"))
	os.Setenv("GOOS", "linux")
	os.Setenv("GOARCH", device.Arch)

	os.Setenv("CC", "clang")
	os.Setenv("CXX", "clang++")
	os.Setenv("LD", "lld")
	os.Setenv("RANLIB", "llvm-ranlib")
	os.Setenv("STRIP", "llvm-strip")
	os.Setenv("AR", "llvm-ar")

	os.Setenv("ARCH", device.Arch)
	os.Setenv("CARCH", device.ArchAlias())
	os.Setenv("TARGET_TRIPLE", device.TargetTriple())
	os.Setenv("SYSROOT", sysrootPath)
	os.Setenv("SOURCES_PATH", sourcesPath)
	os.Setenv("IMAGES_PATH", imagesPath)
	os.Setenv("DEVICE_PATH", devicePath)
	os.Setenv("SYSTEM_PATH", systemPath)
	os.Setenv("KERNEL_VERSION", kernelVersion)

	var components []External
	ensure.Success(
		filepath.Walk(filepath.Join(projectPath, "external"),
			func(path string, info fs.FileInfo, err error) error {
				if err != nil || info.IsDir() || filepath.Base(path) != "build.json" {
					return err
				}
				ext, err := LoadExternal(path)
				if err != nil {
					return fmt.Errorf("%v: %v", path, err)
				}
				components = append(components, ext)
				return nil
			}), "failed to load external components")

	kernelImage := filepath.Join(imagesPath, "kernel.img")
	ensure.Success(func() error {
		ext, err := Sort(components, []string{
			kernelImage,
		})
		if err != nil {
			return err
		}
		components = ext
		return nil
	}(), "failed to sort components")

	var systemImageDependencies []string

	for _, c := range components {
		ensure.Success(
			ensure.Target(c.Provides, c.Build, c.Depends...),
			"failed to build external component")
	}

	for _, dir := range []string{"cmd", "service", "apps"} {
		pkgs, err := os.ReadDir(filepath.Join(projectPath, dir))
		if err != nil {
			continue
		}

		for _, pkg := range pkgs {
			depends := getComponentDepends(filepath.Join(dir, pkg.Name()))

			os.Setenv("CGO_ENABLED", "0")
			target := filepath.Join(systemPath, dir, pkg.Name())
			if idx := slices.IndexFunc(depends, func(f string) bool {
				return filepath.Base(f) == "cgo.go"
			}); idx != -1 {
				os.Setenv("CGO_ENABLED", "1")
			}

			ensure.Success(
				ensure.Target(target,
					ensure.Cmd("go", "build", "-o", target, fmt.Sprintf("chillos/%s/%s", dir, pkg.Name())),
					depends...,
				),
				"failed to build rlxos.dev/%s/%s", dir, pkg.Name())

			systemImageDependencies = append(systemImageDependencies, target)
		}
	}

	systemImage := filepath.Join(imagesPath, "system.img")
	for _, dir := range []string{"config", "data"} {
		systemImageDependencies = append(systemImageDependencies, listFilesRecursive(filepath.Join(projectPath, dir))...)
	}
	ensure.Success(
		ensure.Target(systemImage,
			ensure.Script(
				ensure.Cmd("rsync", "-a", "--delete", projectPath+"/config/", systemPath+"/config/"),
				ensure.Cmd("rsync", "-a", "--delete", projectPath+"/data/", systemPath+"/data/"),
				ensure.Cmd("env", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH, "go", "run", "chillos/cmd/module", "-root", systemPath, "-kernel", kernelVersion, "cache"),
				ensure.Cmd("mksquashfs", systemPath, systemImage, "-noappend", "-all-root"),
			),
			systemImageDependencies...,
		),
		"failed to build system image")

	initramfsImage := filepath.Join(imagesPath, "initramfs.img")
	ensure.Success(
		ensure.Target(initramfsImage,
			ensure.Script(
				ensure.Cmd("install", "-v", "-D", "-m0755", filepath.Join(systemPath, "cmd", "init"), filepath.Join(initramfsPath, "init")),
				ensure.Cmd("sh", "-e", "-c", "cd "+initramfsPath+" && find . -print0 | cpio --null -ov --format=newc --quiet 2>/dev/null >"+initramfsImage),
			),
			systemImage),
		"failed to build initramfs image")

	if runTest {
		args := []string{
			"-smp", fmt.Sprint(cpu),
			"-m", fmt.Sprintf("%dM", memory),
			"-kernel", kernelImage,
			"-initrd", initramfsImage,
			"-drive", "file=" + systemImage + ",format=raw",
			"-append", "-rootfs /dev/sda console=tty0 console=ttyS0",
		}
		args = append(args, device.Emulation...)

		if debug {
			args = append(args, "-serial", "tcp::5555,server")
		}

		if _, err := os.Stat("/dev/kvm"); err == nil {
			args = append(args, "-enable-kvm")
		}

		if vnc >= 0 {
			args = append(args, "-vnc", fmt.Sprintf(":%d", vnc))
		}

		ensure.Success(ensure.Cmd("qemu-system-"+device.ArchAlias(), args...)(),
			"failed to run qemu")

	}
}

func prepareDirectories() error {
	if cachePath == "" {
		cachePath = filepath.Join(projectPath, "_cache")
	}

	if devicePath == "" {
		return fmt.Errorf("no device path specified")
	} else if devicePath[0] != '/' {
		devicePath = filepath.Join(projectPath, "devices", devicePath)
	}

	if err := LoadDeviceConfig(filepath.Join(devicePath, "config.json")); err != nil {
		return fmt.Errorf("LoadDeviceConfig: %v", err)
	}

	deviceCachePath = filepath.Join(cachePath, device.Name)
	toolchainPath = filepath.Join(deviceCachePath, "toolchain")
	systemPath = filepath.Join(deviceCachePath, "system")
	initramfsPath = filepath.Join(deviceCachePath, "initramfs")
	kernelPath = filepath.Join(deviceCachePath, "kernel")
	imagesPath = filepath.Join(deviceCachePath, "images")
	buildPath = filepath.Join(deviceCachePath, "build")
	sourcesPath = filepath.Join(cachePath, "sources")

	sysrootPath = filepath.Join(toolchainPath, device.TargetTriple())

	if clean {
		log.Println("cleaning build")
		for _, dir := range []string{
			systemPath,
			initramfsPath,
			imagesPath,
		} {
			os.RemoveAll(dir)
		}
	}

	for _, p := range []string{
		deviceCachePath, toolchainPath, systemPath,
		initramfsPath, kernelPath, imagesPath, buildPath,
		sourcesPath,
	} {
		ensure.Success(os.MkdirAll(p, 0755), "failed to create %v", p)
	}

	return nil
}

func checkup() error {
	var missing []string
	for _, bin := range []string{
		"go", "rsync", "wget", "mksquashfs", "flex",
		"bison", "bc", "cpio", "make",
	} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}

	for _, header := range []string{
		"gelf.h",
		"openssl/ssl.h",
	} {
		if _, err := os.Stat(filepath.Join("/usr/include", header)); err != nil {
			missing = append(missing, header)
		}
	}
	if missing != nil {
		return fmt.Errorf("missing %v", missing)
	}
	return nil
}

func listFilesRecursive(s string) []string {
	var files []string
	filepath.Walk(s, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		files = append(files, path)
		return nil
	})
	return files
}

func getComponentDepends(id string) []string {
	out, err := exec.Command("go", "list", "-deps", "chillos/"+id).CombinedOutput()
	if err != nil {
		return nil
	}

	var depends []string
	for l := range strings.SplitSeq(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}

		if strings.HasPrefix(l, "chillos/") {
			path := filepath.Join(projectPath, strings.TrimPrefix(l, "chillos/"))
			files, err := os.ReadDir(path)
			if err == nil {
				for _, file := range files {
					if !file.IsDir() {
						depends = append(depends, filepath.Join(path, file.Name()))
					}
				}
			}
		}
	}

	return depends
}
