// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// minimal_vdso emits a relocation-free Linux ELF VDSO without requiring a
// Linux linker on the build host. Its entry points deliberately use syscalls;
// the optimized Linux build remains the default on Linux build hosts.
package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
)

const (
	prelink          = uint64(0xffffffffff700000)
	textSectionIndex = 7
	sectionCount     = 11
)

type function struct {
	names  []string
	code   []byte
	offset uint64
}

type section struct {
	name               string
	type_              uint32
	flags              uint64
	offset, size       uint64
	link, info         uint32
	alignment, entsize uint64
}

func main() {
	arch := flag.String("arch", "", "guest architecture: arm64 or amd64")
	output := flag.String("output", "", "output ELF path")
	flag.Parse()
	if *output == "" {
		fatalf("-output is required")
	}
	machine, version, functions := architecture(*arch)
	image := build(machine, version, functions)
	if err := validate(image, functions); err != nil {
		fatalf("validating generated VDSO: %v", err)
	}
	if err := os.WriteFile(*output, image, 0o644); err != nil {
		fatalf("writing VDSO: %v", err)
	}
}

func architecture(arch string) (uint16, string, []function) {
	switch arch {
	case "arm64":
		return uint16(elf.EM_AARCH64), "LINUX_2.6.39", []function{
			{names: []string{"__kernel_clock_gettime"}, code: arm64Syscall(113, true)},
			{names: []string{"__kernel_gettimeofday"}, code: arm64Syscall(169, true)},
			{names: []string{"__kernel_clock_getres"}, code: arm64Syscall(114, true)},
			{names: []string{"__kernel_rt_sigreturn"}, code: arm64Syscall(139, false)},
		}
	case "amd64":
		return uint16(elf.EM_X86_64), "LINUX_2.6", []function{
			{names: []string{"__vdso_clock_gettime", "clock_gettime"}, code: amd64Syscall(228, true)},
			{names: []string{"__vdso_gettimeofday", "gettimeofday"}, code: amd64Syscall(96, true)},
			{names: []string{"__vdso_time", "time"}, code: amd64Syscall(201, true)},
			{names: []string{"__vdso_getcpu", "getcpu"}, code: amd64Syscall(309, true)},
			{names: []string{"__kernel_rt_sigreturn"}, code: amd64Syscall(15, false)},
		}
	default:
		fatalf("unsupported architecture %q", arch)
		return 0, "", nil
	}
}

func arm64Syscall(number uint32, returns bool) []byte {
	words := []uint32{0xd2800008 | number<<5, 0xd4000001}
	if returns {
		words = append(words, 0xd65f03c0)
	} else {
		words = append(words, 0xd4200000)
	}
	code := make([]byte, len(words)*4)
	for i, word := range words {
		binary.LittleEndian.PutUint32(code[i*4:], word)
	}
	return code
}

func amd64Syscall(number uint32, returns bool) []byte {
	code := []byte{0xb8, byte(number), byte(number >> 8), byte(number >> 16), byte(number >> 24), 0x0f, 0x05}
	if returns {
		return append(code, 0xc3)
	}
	return append(code, 0x0f, 0x0b)
}

func build(machine uint16, version string, functions []function) []byte {
	var symbolNames []string
	for _, fn := range functions {
		symbolNames = append(symbolNames, fn.names...)
	}
	dynstr, nameOffsets := stringTable(append(append([]string{}, symbolNames...), version, "linux-vdso.so.1"))
	image := make([]byte, 64+2*56)
	sections := make([]section, sectionCount)

	sections[1] = appendSection(&image, ".hash", 5, 2, sysvHashTable(symbolNames), 8, 4)
	dynsymOffset := alignImage(&image, 8)
	dynsymSize := uint64((len(symbolNames) + 1) * 24)
	image = append(image, make([]byte, dynsymSize)...)
	sections[2] = section{name: ".dynsym", type_: 11, flags: 2, offset: dynsymOffset, size: dynsymSize, link: 3, info: 1, alignment: 8, entsize: 24}
	sections[3] = appendSection(&image, ".dynstr", 3, 2, dynstr, 1, 0)
	versym := make([]byte, (len(symbolNames)+1)*2)
	for i := 1; i <= len(symbolNames); i++ {
		binary.LittleEndian.PutUint16(versym[i*2:], 2)
	}
	sections[4] = appendSection(&image, ".gnu.version", 0x6fffffff, 2, versym, 2, 2)
	verdef := make([]byte, 28)
	binary.LittleEndian.PutUint16(verdef[0:], 1)
	binary.LittleEndian.PutUint16(verdef[4:], 2)
	binary.LittleEndian.PutUint16(verdef[6:], 1)
	binary.LittleEndian.PutUint32(verdef[8:], elfHash(version))
	binary.LittleEndian.PutUint32(verdef[12:], 20)
	binary.LittleEndian.PutUint32(verdef[20:], nameOffsets[version])
	sections[5] = appendSection(&image, ".gnu.version_d", 0x6ffffffd, 2, verdef, 4, 0)
	sections[5].link = 3
	sections[5].info = 1
	dynamicOffset := alignImage(&image, 8)
	const dynamicEntries = 10
	image = append(image, make([]byte, dynamicEntries*16)...)
	sections[6] = section{name: ".dynamic", type_: 6, flags: 2, offset: dynamicOffset, size: dynamicEntries * 16, link: 3, alignment: 8, entsize: 16}

	textOffset := alignImage(&image, 4096)
	for i := range functions {
		alignImage(&image, 16)
		functions[i].offset = uint64(len(image)) - textOffset
		image = append(image, functions[i].code...)
	}
	sections[7] = section{name: ".text", type_: 1, flags: 6, offset: textOffset, size: uint64(len(image)) - textOffset, alignment: 16}

	fillSymbols(image[dynsymOffset:dynsymOffset+dynsymSize], functions, nameOffsets, textOffset)
	symtab := make([]byte, dynsymSize)
	copy(symtab, image[dynsymOffset:dynsymOffset+dynsymSize])
	sections[8] = appendSection(&image, ".symtab", 2, 0, symtab, 8, 24)
	sections[8].link = 9
	sections[8].info = 1
	sections[9] = appendSection(&image, ".strtab", 3, 0, dynstr, 1, 0)
	shstr, shname := stringTable([]string{".hash", ".dynsym", ".dynstr", ".gnu.version", ".gnu.version_d", ".dynamic", ".text", ".symtab", ".strtab", ".shstrtab"})
	sections[10] = appendSection(&image, ".shstrtab", 3, 0, shstr, 1, 0)
	sectionHeaderOffset := alignImage(&image, 8)
	image = append(image, make([]byte, sectionCount*64)...)

	fillDynamic(image[dynamicOffset:dynamicOffset+dynamicEntries*16], sections, uint64(len(dynstr)), nameOffsets["linux-vdso.so.1"])
	fillSectionHeaders(image[sectionHeaderOffset:], sections, shname)
	fillELFHeader(image, machine, sectionHeaderOffset)
	fillProgramHeader(image[64:], 1, 5, 0, prelink, uint64(len(image)), uint64(len(image)), 4096)
	fillProgramHeader(image[64+56:], 2, 4, dynamicOffset, prelink+dynamicOffset, dynamicEntries*16, dynamicEntries*16, 8)
	return image
}

func appendSection(image *[]byte, name string, typ uint32, flags uint64, data []byte, alignment, entsize uint64) section {
	offset := alignImage(image, alignment)
	*image = append(*image, data...)
	return section{name: name, type_: typ, flags: flags, offset: offset, size: uint64(len(data)), alignment: alignment, entsize: entsize}
}

func alignImage(image *[]byte, alignment uint64) uint64 {
	for uint64(len(*image))%alignment != 0 {
		*image = append(*image, 0)
	}
	return uint64(len(*image))
}

func stringTable(strings []string) ([]byte, map[string]uint32) {
	data := []byte{0}
	offsets := make(map[string]uint32)
	for _, value := range strings {
		if _, ok := offsets[value]; ok {
			continue
		}
		offsets[value] = uint32(len(data))
		data = append(data, value...)
		data = append(data, 0)
	}
	return data, offsets
}

func fillSymbols(dst []byte, functions []function, names map[string]uint32, textOffset uint64) {
	index := 1
	for _, fn := range functions {
		for _, name := range fn.names {
			symbol := dst[index*24:]
			binary.LittleEndian.PutUint32(symbol[0:], names[name])
			symbol[4] = byte(elf.STB_GLOBAL)<<4 | byte(elf.STT_FUNC)
			binary.LittleEndian.PutUint16(symbol[6:], textSectionIndex)
			binary.LittleEndian.PutUint64(symbol[8:], prelink+textOffset+fn.offset)
			binary.LittleEndian.PutUint64(symbol[16:], uint64(len(fn.code)))
			index++
		}
	}
}

func sysvHashTable(names []string) []byte {
	data := make([]byte, (2+1+len(names)+1)*4)
	binary.LittleEndian.PutUint32(data[0:], 1)
	binary.LittleEndian.PutUint32(data[4:], uint32(len(names)+1))
	if len(names) != 0 {
		binary.LittleEndian.PutUint32(data[8:], 1)
	}
	chains := data[12:]
	for i := 1; i < len(names); i++ {
		binary.LittleEndian.PutUint32(chains[i*4:], uint32(i+1))
	}
	return data
}

func elfHash(value string) uint32 {
	var hash uint32
	for i := 0; i < len(value); i++ {
		hash = hash<<4 + uint32(value[i])
		g := hash & 0xf0000000
		if g != 0 {
			hash ^= g >> 24
		}
		hash &^= g
	}
	return hash
}

func fillDynamic(dst []byte, sections []section, strsz uint64, soname uint32) {
	entries := [][2]uint64{
		{4, prelink + sections[1].offset},
		{5, prelink + sections[3].offset},
		{6, prelink + sections[2].offset},
		{10, strsz}, {11, 24},
		{0x6ffffff0, prelink + sections[4].offset},
		{0x6ffffffc, prelink + sections[5].offset},
		{0x6ffffffd, 1}, {14, uint64(soname)}, {0, 0},
	}
	for i, entry := range entries {
		binary.LittleEndian.PutUint64(dst[i*16:], entry[0])
		binary.LittleEndian.PutUint64(dst[i*16+8:], entry[1])
	}
}

func fillSectionHeaders(dst []byte, sections []section, names map[string]uint32) {
	for i := 1; i < len(sections); i++ {
		out := dst[i*64:]
		s := sections[i]
		binary.LittleEndian.PutUint32(out[0:], names[s.name])
		binary.LittleEndian.PutUint32(out[4:], s.type_)
		binary.LittleEndian.PutUint64(out[8:], s.flags)
		if s.flags&2 != 0 {
			binary.LittleEndian.PutUint64(out[16:], prelink+s.offset)
		}
		binary.LittleEndian.PutUint64(out[24:], s.offset)
		binary.LittleEndian.PutUint64(out[32:], s.size)
		binary.LittleEndian.PutUint32(out[40:], s.link)
		binary.LittleEndian.PutUint32(out[44:], s.info)
		binary.LittleEndian.PutUint64(out[48:], s.alignment)
		binary.LittleEndian.PutUint64(out[56:], s.entsize)
	}
}

func fillELFHeader(image []byte, machine uint16, shoff uint64) {
	copy(image[:16], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	binary.LittleEndian.PutUint16(image[16:], uint16(elf.ET_DYN))
	binary.LittleEndian.PutUint16(image[18:], machine)
	binary.LittleEndian.PutUint32(image[20:], 1)
	binary.LittleEndian.PutUint64(image[32:], 64)
	binary.LittleEndian.PutUint64(image[40:], shoff)
	binary.LittleEndian.PutUint16(image[52:], 64)
	binary.LittleEndian.PutUint16(image[54:], 56)
	binary.LittleEndian.PutUint16(image[56:], 2)
	binary.LittleEndian.PutUint16(image[58:], 64)
	binary.LittleEndian.PutUint16(image[60:], sectionCount)
	binary.LittleEndian.PutUint16(image[62:], 10)
}

func fillProgramHeader(dst []byte, typ, flags uint32, offset, address, filesz, memsz, alignment uint64) {
	binary.LittleEndian.PutUint32(dst[0:], typ)
	binary.LittleEndian.PutUint32(dst[4:], flags)
	binary.LittleEndian.PutUint64(dst[8:], offset)
	binary.LittleEndian.PutUint64(dst[16:], address)
	binary.LittleEndian.PutUint64(dst[24:], address)
	binary.LittleEndian.PutUint64(dst[32:], filesz)
	binary.LittleEndian.PutUint64(dst[40:], memsz)
	binary.LittleEndian.PutUint64(dst[48:], alignment)
}

func validate(image []byte, functions []function) error {
	f, err := elf.NewFile(bytes.NewReader(image))
	if err != nil {
		return err
	}
	defer f.Close()
	if len(f.Progs) == 0 || f.Progs[0].Type != elf.PT_LOAD || f.Progs[0].Off != 0 || f.Progs[0].Filesz != f.Progs[0].Memsz {
		return fmt.Errorf("invalid load segment")
	}
	if len(f.Sections) != sectionCount || f.Section(".text") == nil || f.Section(".text").Size == 0 {
		return fmt.Errorf("invalid section table")
	}
	symbols, err := f.Symbols()
	if err != nil {
		return err
	}
	want := 0
	for _, fn := range functions {
		want += len(fn.names)
	}
	if len(symbols) != want {
		return fmt.Errorf("got %d symbols, want %d", len(symbols), want)
	}
	return nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "minimal_vdso: "+format+"\n", args...)
	os.Exit(1)
}
