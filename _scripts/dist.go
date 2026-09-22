// Command dist is the part of the release packaging that a shell cannot do
// portably: writing a zip archive, writing and checking a SHA256SUMS file, and
// deciding whether a binary built for another platform is statically linked.
//
// It lives in _scripts so the go tool never matches it with ./..., and it uses
// the standard library only, so `make dist` needs nothing but a Go toolchain
// and tar.
//
// Usage:
//
//	go run _scripts/dist.go zip   <archive.zip> <file>...
//	go run _scripts/dist.go sums  <dir>
//	go run _scripts/dist.go check <dir>
//	go run _scripts/dist.go static <binary> <goos> <goarch>
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: dist zip|sums|check|static ..."))
	}
	var err error
	switch os.Args[1] {
	case "zip":
		if len(os.Args) < 4 {
			fail(fmt.Errorf("usage: dist zip <archive.zip> <file>..."))
		}
		err = writeZip(os.Args[2], os.Args[3:])
	case "sums":
		if len(os.Args) != 3 {
			fail(fmt.Errorf("usage: dist sums <dir>"))
		}
		err = writeSums(os.Args[2])
	case "check":
		if len(os.Args) != 3 {
			fail(fmt.Errorf("usage: dist check <dir>"))
		}
		err = checkSums(os.Args[2])
	case "static":
		if len(os.Args) != 5 {
			fail(fmt.Errorf("usage: dist static <binary> <goos> <goarch>"))
		}
		err = checkStatic(os.Args[2], os.Args[3], os.Args[4])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "dist:", err)
	os.Exit(1)
}

// writeZip stores every named file at the top level of the archive, keeping
// the executable bit so a binary unpacked on a unix host stays runnable.
func writeZip(archive string, files []string) error {
	out, err := os.Create(archive)
	if err != nil {
		return err
	}
	defer out.Close()

	w := zip.NewWriter(out)
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.Base(path)
		header.Method = zip.Deflate
		entry, err := w.CreateHeader(header)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(entry, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	return out.Close()
}

// archives are the release artifacts a SHA256SUMS file covers.
func archives(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tar.gz") || strings.HasSuffix(e.Name(), ".zip") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%s: no archives", dir)
	}
	return names, nil
}

func sum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeSums writes dir/SHA256SUMS in the format sha256sum -c reads.
func writeSums(dir string) error {
	names, err := archives(dir)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, name := range names {
		s, err := sum(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s  %s\n", s, name)
	}
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(b.String()), 0o644)
}

// checkSums recomputes every digest in dir/SHA256SUMS and reports the first
// archive that does not match, or one that the file does not cover.
func checkSums(dir string) error {
	f, err := os.Open(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	defer f.Close()

	covered := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		want, name, ok := strings.Cut(line, "  ")
		if !ok {
			return fmt.Errorf("SHA256SUMS: cannot parse %q", line)
		}
		got, err := sum(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s: sha256 mismatch", name)
		}
		covered[name] = true
		fmt.Printf("sha256 ok   %s\n", name)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	names, err := archives(dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !covered[name] {
			return fmt.Errorf("%s: not listed in SHA256SUMS", name)
		}
	}
	return nil
}

// checkStatic reports whether the binary is for the named platform and carries
// no dynamic interpreter or shared library dependency. A dynamically linked
// build made on NixOS names its interpreter by an absolute /nix/store path
// that exists on no other machine, so an artifact with one is not shippable.
func checkStatic(path, goos, goarch string) error {
	switch goos {
	case "linux":
		return checkELF(path, goarch)
	case "darwin":
		return checkMachO(path, goarch)
	case "windows":
		return checkPE(path, goarch)
	default:
		return fmt.Errorf("%s: unknown GOOS %q", path, goos)
	}
}

// elfMachines maps GOARCH to the machine an ELF header must name.
var elfMachines = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
	"arm":   elf.EM_ARM,
}

func checkELF(path, goarch string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s: not an ELF binary: %w", path, err)
	}
	defer f.Close()

	want, ok := elfMachines[goarch]
	if !ok {
		return fmt.Errorf("%s: unknown GOARCH %q", path, goarch)
	}
	if f.Machine != want {
		return fmt.Errorf("%s: built for %s, want %s", path, f.Machine, want)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s: has a dynamic interpreter", path)
		}
	}
	if libs, err := f.ImportedLibraries(); err == nil && len(libs) > 0 {
		return fmt.Errorf("%s: needs shared libraries %v", path, libs)
	}
	fmt.Printf("static ok   %s (elf %s)\n", path, f.Machine)
	return nil
}

var machoCPUs = map[string]macho.Cpu{
	"amd64": macho.CpuAmd64,
	"arm64": macho.CpuArm64,
}

// machoSystemPrefixes are the directories of the libraries macOS itself ships.
// A cgo-free Go binary still binds libSystem and a few frameworks from there,
// and they are present on every macOS install, so they are not a dependency
// the artifact has to carry. A library from anywhere else, such as Homebrew or
// a Nix store, is one the target machine would not have.
var machoSystemPrefixes = []string{"/usr/lib/", "/System/Library/"}

func isMachOSystemLib(lib string) bool {
	for _, prefix := range machoSystemPrefixes {
		if strings.HasPrefix(lib, prefix) {
			return true
		}
	}
	return false
}

func checkMachO(path, goarch string) error {
	f, err := macho.Open(path)
	if err != nil {
		return fmt.Errorf("%s: not a Mach-O binary: %w", path, err)
	}
	defer f.Close()

	want, ok := machoCPUs[goarch]
	if !ok {
		return fmt.Errorf("%s: unknown GOARCH %q", path, goarch)
	}
	if f.Cpu != want {
		return fmt.Errorf("%s: built for %s, want %s", path, f.Cpu, want)
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("%s: read imports: %w", path, err)
	}
	for _, lib := range libs {
		if !isMachOSystemLib(lib) {
			return fmt.Errorf("%s: needs shared library %s", path, lib)
		}
	}
	fmt.Printf("static ok   %s (mach-o %s)\n", path, f.Cpu)
	return nil
}

// peImportDirectory is the index of the import table in the optional header's
// data directory, and peImportDescriptorSize the size of one entry in it.
const (
	peImportDirectory      = 1
	peImportDescriptorSize = 20
)

var peMachines = map[string]uint16{
	"amd64": pe.IMAGE_FILE_MACHINE_AMD64,
	"arm64": pe.IMAGE_FILE_MACHINE_ARM64,
}

// peSystemDLLs are the libraries a cgo-free Windows Go binary resolves at
// startup. Anything else means it was linked against a C toolchain.
var peSystemDLLs = map[string]bool{
	"kernel32.dll": true,
}

// peImportedLibraries returns the DLL names in the binary's import directory.
//
// debug/pe has an ImportedLibraries method, but it returns nil, nil for every
// input, so a check written on it can never fail. The import directory is read
// here instead: data directory entry 1 points at a list of 20-byte import
// descriptors, each carrying at offset 12 the address of its DLL name, and the
// list ends at an all-zero descriptor.
func peImportedLibraries(f *pe.File) ([]string, error) {
	var dir pe.DataDirectory
	switch h := f.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		if len(h.DataDirectory) <= peImportDirectory {
			return nil, nil
		}
		dir = h.DataDirectory[peImportDirectory]
	case *pe.OptionalHeader64:
		if len(h.DataDirectory) <= peImportDirectory {
			return nil, nil
		}
		dir = h.DataDirectory[peImportDirectory]
	default:
		return nil, fmt.Errorf("no optional header")
	}
	if dir.VirtualAddress == 0 || dir.Size == 0 {
		return nil, nil
	}

	var libs []string
	for offset := uint32(0); ; offset += peImportDescriptorSize {
		descriptor, err := peReadAt(f, dir.VirtualAddress+offset, peImportDescriptorSize)
		if err != nil {
			return nil, err
		}
		if isZero(descriptor) {
			return libs, nil
		}
		nameRVA := binary.LittleEndian.Uint32(descriptor[12:16])
		if nameRVA == 0 {
			return nil, fmt.Errorf("import descriptor with no name")
		}
		name, err := peReadString(f, nameRVA)
		if err != nil {
			return nil, err
		}
		libs = append(libs, name)
	}
}

// peSectionFor returns the section holding the given address, and the offset
// of that address inside the section's data.
func peSectionFor(f *pe.File, rva uint32) (*pe.Section, uint32, error) {
	for _, s := range f.Sections {
		size := s.VirtualSize
		if size == 0 {
			size = s.Size
		}
		if rva >= s.VirtualAddress && rva < s.VirtualAddress+size {
			return s, rva - s.VirtualAddress, nil
		}
	}
	return nil, 0, fmt.Errorf("address 0x%x is in no section", rva)
}

// peReadAt returns n bytes at the given address.
func peReadAt(f *pe.File, rva, n uint32) ([]byte, error) {
	section, offset, err := peSectionFor(f, rva)
	if err != nil {
		return nil, err
	}
	data, err := section.Data()
	if err != nil {
		return nil, err
	}
	if uint64(offset)+uint64(n) > uint64(len(data)) {
		return nil, fmt.Errorf("address 0x%x runs past section %s", rva, section.Name)
	}
	return data[offset : offset+n], nil
}

// peReadString returns the NUL-terminated string at the given address.
func peReadString(f *pe.File, rva uint32) (string, error) {
	section, offset, err := peSectionFor(f, rva)
	if err != nil {
		return "", err
	}
	data, err := section.Data()
	if err != nil {
		return "", err
	}
	if uint64(offset) >= uint64(len(data)) {
		return "", fmt.Errorf("address 0x%x runs past section %s", rva, section.Name)
	}
	rest := data[offset:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return "", fmt.Errorf("unterminated string at 0x%x", rva)
	}
	return string(rest[:end]), nil
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func checkPE(path, goarch string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("%s: not a PE binary: %w", path, err)
	}
	defer f.Close()

	want, ok := peMachines[goarch]
	if !ok {
		return fmt.Errorf("%s: unknown GOARCH %q", path, goarch)
	}
	if f.Machine != want {
		return fmt.Errorf("%s: built for machine 0x%x, want 0x%x", path, f.Machine, want)
	}
	libs, err := peImportedLibraries(f)
	if err != nil {
		return fmt.Errorf("%s: read imports: %w", path, err)
	}
	for _, lib := range libs {
		if !peSystemDLLs[strings.ToLower(lib)] {
			return fmt.Errorf("%s: imports %s", path, lib)
		}
	}
	fmt.Printf("static ok   %s (pe 0x%x, imports %v)\n", path, f.Machine, libs)
	return nil
}
