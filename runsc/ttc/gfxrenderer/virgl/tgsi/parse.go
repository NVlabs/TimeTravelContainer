// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tgsi

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Limits of what Parse takes, which no shader that a driver sends nears.
const (
	// MaxIndex bounds registers' indexes and ranges, as TGSI's tokens
	// hold them in 16 bits.
	MaxIndex = 1<<16 - 1
	// MaxTokens bounds a shader's properties, declarations, immediates and
	// instructions together.
	MaxTokens = 1 << 20
)

// SyntaxError is an error of a shader's text, at a line and column.
type SyntaxError struct {
	Line, Column int
	Msg          string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("TGSI %d:%d: %s", e.Line, e.Column, e.Msg)
}

// Parse parses a shader's text, as Mesa's tgsi_text_translate does, but for
// floats printed as hex, which it reads as their bits.
func Parse(text string) (*Shader, error) {
	p := &parser{text: text}
	s, err := p.shader()
	if err != nil {
		return nil, err
	}
	return s, nil
}

type parser struct {
	text string
	pos  int
	s    *Shader
	// tokens is the number of the shader's tokens parsed, of MaxTokens.
	tokens int
}

// fail returns a SyntaxError at the parser's position.
func (p *parser) fail(format string, args ...any) error {
	line, col := 1, 1
	for i := 0; i < p.pos && i < len(p.text); i++ {
		if p.text[i] == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return &SyntaxError{Line: line, Column: col, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) peek() byte {
	if p.pos < len(p.text) {
		return p.text[p.pos]
	}
	return 0
}

// skipSpace skips spaces and tabs, and returns whether it skipped any. Mesa
// also takes comments: none that a driver prints.
func (p *parser) skipSpace() bool {
	start := p.pos
	for p.pos < len(p.text) {
		switch p.text[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		case '/':
			if p.pos+1 < len(p.text) && p.text[p.pos+1] == '*' {
				end := strings.Index(p.text[p.pos+2:], "*/")
				if end < 0 {
					p.pos = len(p.text)
					return true
				}
				p.pos += end + 4
				continue
			}
			return p.pos > start
		case ';':
			// A comment to the end of the line.
			for p.pos < len(p.text) && p.text[p.pos] != '\n' {
				p.pos++
			}
		default:
			return p.pos > start
		}
	}
	return p.pos > start
}

// skipBlanks skips spaces and tabs only.
func (p *parser) skipBlanks() {
	for p.pos < len(p.text) && (p.text[p.pos] == ' ' || p.text[p.pos] == '\t') {
		p.pos++
	}
}

// expect skips blanks and then c.
func (p *parser) expect(c byte) error {
	p.skipBlanks()
	if p.peek() != c {
		return p.fail("expected %q", c)
	}
	p.pos++
	return nil
}

func isIdentByte(c byte, first bool) bool {
	switch {
	case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return !first
	}
	return false
}

// word returns the identifier at the parser's position, without consuming
// it, as Mesa's str_match_nocase_whole matches it: letters, digits and
// underscores, case aside.
func (p *parser) word() string {
	end := p.pos
	for end < len(p.text) && isIdentByte(p.text[end], end == p.pos) {
		end++
	}
	return p.text[p.pos:end]
}

// matchWord consumes w, of any case, if the text at the parser's position
// is w, whole, as Mesa's str_match_nocase_whole matches it: not followed by
// a letter, digit or underscore. Names may begin with digits, as 2D.
func (p *parser) matchWord(w string) bool {
	end := p.pos + len(w)
	if end > len(p.text) || !strings.EqualFold(p.text[p.pos:end], w) {
		return false
	}
	if end < len(p.text) && isIdentByte(p.text[end], false) {
		return false
	}
	p.pos = end
	return true
}

// matchEnum consumes the first of names that matchWord matches, and returns
// its index.
func (p *parser) matchEnum(names []string) (int, bool) {
	for i, n := range names {
		if p.matchWord(n) {
			return i, true
		}
	}
	return 0, false
}

// uint parses an unsigned decimal integer.
func (p *parser) uint() (uint64, bool) {
	start := p.pos
	var v uint64
	for p.pos < len(p.text) && p.text[p.pos] >= '0' && p.text[p.pos] <= '9' {
		v = v*10 + uint64(p.text[p.pos]-'0')
		if v > math.MaxUint32 {
			return 0, false
		}
		p.pos++
	}
	return v, p.pos > start
}

// int parses a signed decimal integer.
func (p *parser) int() (int64, bool) {
	start := p.pos
	neg := false
	if c := p.peek(); c == '-' || c == '+' {
		neg = c == '-'
		p.pos++
	}
	v, ok := p.uint()
	if !ok {
		p.pos = start
		return 0, false
	}
	if neg {
		return -int64(v), true
	}
	return int64(v), true
}

// index parses a register's index, of at most MaxIndex.
func (p *parser) index() (int, error) {
	p.skipBlanks()
	v, ok := p.int()
	if !ok {
		return 0, p.fail("expected an index")
	}
	if v < -MaxIndex || v > MaxIndex {
		return 0, p.fail("index %d", v)
	}
	return int(v), nil
}

// number returns the text of a number at the parser's position: digits,
// signs, points, exponents and hex digits, which strconv then checks.
func (p *parser) number() string {
	start := p.pos
	for p.pos < len(p.text) {
		c := p.text[p.pos]
		if c >= '0' && c <= '9' || c == '.' || c == '-' || c == '+' || c == 'x' || c == 'X' ||
			c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == 'n' || c == 'N' || c == 'i' || c == 'I' {
			p.pos++
			continue
		}
		break
	}
	return p.text[start:p.pos]
}

func (p *parser) shader() (*Shader, error) {
	p.s = &Shader{}
	p.skipSpace()
	proc, ok := p.matchEnum(processorNames)
	if !ok {
		return nil, p.fail("unknown header")
	}
	p.s.Processor = Processor(proc)
	for {
		if !p.skipSpace() && p.pos < len(p.text) {
			return nil, p.fail("syntax error")
		}
		if p.pos >= len(p.text) || p.peek() == 0 {
			// A driver's text ends with the string's NUL.
			return p.s, nil
		}
		if p.tokens++; p.tokens > MaxTokens {
			return nil, p.fail("more than %d tokens", MaxTokens)
		}
		if p.label() {
			if err := p.instruction(); err != nil {
				return nil, err
			}
			continue
		}
		switch {
		case p.matchWord("DCL"):
			if err := p.declaration(); err != nil {
				return nil, err
			}
		case p.matchWord("IMM"):
			if err := p.immediate(); err != nil {
				return nil, err
			}
		case p.matchWord("PROPERTY"):
			if err := p.property(); err != nil {
				return nil, err
			}
		default:
			if err := p.instruction(); err != nil {
				return nil, err
			}
		}
	}
}

// label consumes an instruction's number, "N:", which Mesa prints before
// each instruction and ignores.
func (p *parser) label() bool {
	start := p.pos
	if _, ok := p.uint(); ok {
		p.skipBlanks()
		if p.peek() == ':' {
			p.pos++
			return true
		}
	}
	p.pos = start
	return false
}

func (p *parser) property() error {
	if !p.skipSpace() {
		return p.fail("syntax error")
	}
	name, ok := p.matchEnum(propertyNames)
	if !ok {
		return p.fail("unknown property")
	}
	p.skipBlanks()
	prop := Property{Name: PropertyName(name)}
	if names := propertyValueNames(prop.Name); names != nil {
		v, ok := p.matchEnum(names)
		if !ok {
			return p.fail("unknown value of %s", prop.Name)
		}
		prop.Value = uint32(v)
	} else {
		// Mesa prints a value as signed, as one past 2^31 reads.
		v, ok := p.int()
		if !ok || v < math.MinInt32 || v > math.MaxUint32 {
			return p.fail("expected an integer")
		}
		prop.Value = uint32(v)
	}
	p.s.Order = append(p.s.Order, TokenRef{TokenProperty, len(p.s.Properties)})
	p.s.Properties = append(p.s.Properties, prop)
	return nil
}

// file parses a register file's name.
func (p *parser) file() (File, error) {
	f, ok := p.matchEnum(fileNames)
	if !ok {
		return 0, p.fail("unknown register file")
	}
	return File(f), nil
}

// writeMask parses an optional write mask, of the channels in order.
func (p *parser) writeMask() (uint8, error) {
	p.skipBlanks()
	if p.peek() != '.' {
		return WriteMaskXYZW, nil
	}
	p.pos++
	p.skipBlanks()
	var mask uint8
	for i, c := range "xyzw" {
		if p.pos < len(p.text) && (rune(p.text[p.pos]) == c || rune(p.text[p.pos]) == c-'a'+'A') {
			mask |= 1 << i
			p.pos++
		}
	}
	if mask == 0 {
		return 0, p.fail("expected a write mask")
	}
	return mask, nil
}

// swizzleChannel parses a channel's name.
func (p *parser) swizzleChannel() (uint8, bool) {
	switch p.peek() {
	case 'x', 'X':
		p.pos++
		return X, true
	case 'y', 'Y':
		p.pos++
		return Y, true
	case 'z', 'Z':
		p.pos++
		return Z, true
	case 'w', 'W':
		p.pos++
		return W, true
	}
	return 0, false
}

// declRange parses "[first]" or "[first..last]", or "[]", for which empty
// is true.
func (p *parser) declRange() (first, last int, empty bool, err error) {
	if err := p.expect('['); err != nil {
		return 0, 0, false, err
	}
	p.skipBlanks()
	if p.peek() == ']' {
		p.pos++
		return 0, 0, true, nil
	}
	if first, err = p.index(); err != nil {
		return
	}
	last = first
	p.skipBlanks()
	if strings.HasPrefix(p.text[p.pos:], "..") {
		p.pos += 2
		if last, err = p.index(); err != nil {
			return
		}
	}
	if err = p.expect(']'); err != nil {
		return
	}
	if first < 0 || last < first {
		err = p.fail("range [%d..%d]", first, last)
	}
	return
}

func (p *parser) declaration() error {
	if !p.skipSpace() {
		return p.fail("syntax error")
	}
	var d Declaration
	var err error
	if d.File, err = p.file(); err != nil {
		return err
	}
	// Up to two brackets, after any "[]" of a two-dimensional input or
	// output: the second dimension's index, and the range.
	var brackets [][2]int
	for {
		p.skipBlanks()
		if p.peek() != '[' {
			break
		}
		first, last, empty, err := p.declRange()
		if err != nil {
			return err
		}
		if empty {
			continue
		}
		brackets = append(brackets, [2]int{first, last})
	}
	switch len(brackets) {
	case 1:
		d.First, d.Last = brackets[0][0], brackets[0][1]
	case 2:
		d.Dimension = true
		d.Index2D = brackets[0][0]
		d.First, d.Last = brackets[1][0], brackets[1][1]
	default:
		return p.fail("expected a register range")
	}
	if d.UsageMask, err = p.writeMask(); err != nil {
		return err
	}
	isVSInput := d.File == FileInput && p.s.Processor == ProcessorVertex
	// The attributes, in Mesa's order, each after a comma.
	next := func() bool {
		save := p.pos
		p.skipBlanks()
		if p.peek() != ',' {
			p.pos = save
			return false
		}
		p.pos++
		p.skipBlanks()
		return true
	}
	if save := p.pos; next() {
		if p.matchWord("ARRAY") {
			if err := p.expect('('); err != nil {
				return err
			}
			id, err := p.index()
			if err != nil {
				return err
			}
			if err := p.expect(')'); err != nil {
				return err
			}
			d.ArrayID = id
		} else {
			p.pos = save
		}
	}
	if save := p.pos; !isVSInput && next() {
		switch d.File {
		case FileImage:
			t, ok := p.matchEnum(textureNames)
			if !ok {
				return p.fail("expected a texture target")
			}
			d.ImageTarget = TextureTarget(t)
			for {
				save := p.pos
				if !next() {
					break
				}
				switch {
				case p.matchWord("RAW"):
					d.ImageRaw = true
				case p.matchWord("WR"):
					d.ImageWritable = true
				case strings.HasPrefix(strings.ToUpper(p.word()), "PIPE_FORMAT_"):
					d.ImageFormat = p.word()
					p.pos += len(d.ImageFormat)
				default:
					p.pos = save
				}
				if p.pos == save {
					break
				}
			}
		case FileSamplerView:
			t, ok := p.matchEnum(textureNames)
			if !ok {
				return p.fail("expected a texture target")
			}
			d.ViewTarget = TextureTarget(t)
			if err := p.expect(','); err != nil {
				return err
			}
			p.skipBlanks()
			n := 0
			for n < 4 {
				r, ok := p.matchEnum(returnTypeNames)
				if !ok {
					if n == 0 || n > 2 {
						return p.fail("expected a type")
					}
					break
				}
				d.ReturnTypes[n] = ReturnType(r)
				n++
				save := p.pos
				if !next() {
					break
				}
				if r, ok := p.matchEnum(returnTypeNames); ok {
					// Matched again by the loop.
					p.pos -= len(returnTypeNames[r])
				} else {
					p.pos = save
					break
				}
			}
			if n < 4 {
				for i := 1; i < 4; i++ {
					d.ReturnTypes[i] = d.ReturnTypes[0]
				}
			}
		case FileBuffer:
			if p.matchWord("ATOMIC") {
				d.Atomic = true
			} else {
				p.pos = save
			}
		case FileMemory:
			if t, ok := p.matchEnum(memoryTypeNames); ok {
				d.MemoryType = MemoryType(t)
			} else {
				p.pos = save
			}
		default:
			if p.matchWord("LOCAL") {
				d.Local = true
			} else {
				p.pos = save
			}
			if save := p.pos; next() {
				if sem, ok := p.matchEnum(semanticNames); ok {
					d.HasSemantic = true
					d.Semantic = Semantic(sem)
					p.skipBlanks()
					if p.peek() == '[' {
						p.pos++
						p.skipBlanks()
						idx, ok := p.uint()
						if !ok || idx > MaxIndex {
							return p.fail("expected a semantic index")
						}
						if err := p.expect(']'); err != nil {
							return err
						}
						d.SemanticIndex = int(idx)
					}
				} else {
					p.pos = save
				}
			}
		}
	}
	if save := p.pos; d.File == FileOutput && p.s.Processor == ProcessorGeometry && next() {
		if p.matchWord("STREAM") {
			if err := p.expect('('); err != nil {
				return err
			}
			for i := 0; i < 4; i++ {
				if i > 0 {
					if err := p.expect(','); err != nil {
						return err
					}
				}
				p.skipBlanks()
				v, ok := p.uint()
				if !ok || v > 3 {
					return p.fail("expected a stream")
				}
				d.Streams[i] = uint8(v)
			}
			if err := p.expect(')'); err != nil {
				return err
			}
		} else {
			p.pos = save
		}
	}
	if save := p.pos; !isVSInput && next() {
		if i, ok := p.matchEnum(interpolationNames); ok {
			d.Interpolate = true
			d.Interp = Interpolation(i)
		} else {
			p.pos = save
		}
	}
	if save := p.pos; !isVSInput && next() {
		if l, ok := p.matchEnum(locationNames); ok {
			// Mesa keeps a location only of an input that interpolates.
			d.Location = InterpolationLocation(l)
		} else {
			p.pos = save
		}
	}
	if !isVSInput && next() {
		if !p.matchWord("INVARIANT") {
			return p.fail("expected a semantic, an interpolation, or INVARIANT")
		}
		d.Invariant = true
	}
	p.s.Order = append(p.s.Order, TokenRef{TokenDeclaration, len(p.s.Declarations)})
	p.s.Declarations = append(p.s.Declarations, d)
	return nil
}

func (p *parser) immediate() error {
	if p.peek() == '[' {
		p.pos++
		p.skipBlanks()
		idx, ok := p.uint()
		if !ok {
			return p.fail("expected an immediate's index")
		}
		if int(idx) != len(p.s.Immediates) {
			return p.fail("immediates must be in order")
		}
		if err := p.expect(']'); err != nil {
			return err
		}
	}
	if !p.skipSpace() {
		return p.fail("syntax error")
	}
	t, ok := p.matchEnum(immediateTypeNames)
	if !ok {
		return p.fail("expected an immediate's type")
	}
	imm := Immediate{Type: ImmediateType(t)}
	if err := p.expect('{'); err != nil {
		return err
	}
	// Mesa's immediates have four 32-bit values, of which a 64-bit value
	// takes two.
	for i := 0; i < 4; i++ {
		p.skipBlanks()
		if i > 0 {
			if p.peek() != ',' {
				return p.fail("expected ','")
			}
			p.pos++
			p.skipBlanks()
		}
		text := p.number()
		switch imm.Type {
		case ImmFloat32:
			v, err := parseFloat32(text)
			if err != nil {
				return p.fail("immediate %q: %v", text, err)
			}
			imm.Values = append(imm.Values, v)
		case ImmUint32:
			v, err := strconv.ParseUint(text, 10, 32)
			if err != nil {
				return p.fail("immediate %q: %v", text, err)
			}
			imm.Values = append(imm.Values, uint32(v))
		case ImmInt32:
			v, err := strconv.ParseInt(text, 10, 32)
			if err != nil {
				return p.fail("immediate %q: %v", text, err)
			}
			imm.Values = append(imm.Values, uint32(int32(v)))
		case ImmFloat64:
			v, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return p.fail("immediate %q: %v", text, err)
			}
			b := math.Float64bits(v)
			imm.Values = append(imm.Values, uint32(b), uint32(b>>32))
			i++
		case ImmUint64:
			v, err := strconv.ParseUint(text, 0, 64)
			if err != nil {
				return p.fail("immediate %q: %v", text, err)
			}
			imm.Values = append(imm.Values, uint32(v), uint32(v>>32))
			i++
		case ImmInt64:
			v, err := strconv.ParseInt(text, 0, 64)
			if err != nil {
				return p.fail("immediate %q: %v", text, err)
			}
			imm.Values = append(imm.Values, uint32(uint64(v)), uint32(uint64(v)>>32))
			i++
		}
	}
	if err := p.expect('}'); err != nil {
		return err
	}
	p.s.Order = append(p.s.Order, TokenRef{TokenImmediate, len(p.s.Immediates)})
	p.s.Immediates = append(p.s.Immediates, imm)
	return nil
}

// parseFloat32 parses a float of an immediate: its bits, in hex, as
// TGSI_DUMP_FLOAT_AS_HEX prints them, or a decimal.
func parseFloat32(text string) (uint32, error) {
	if len(text) > 2 && text[0] == '0' && (text[1] == 'x' || text[1] == 'X') {
		v, err := strconv.ParseUint(text[2:], 16, 32)
		return uint32(v), err
	}
	v, err := strconv.ParseFloat(text, 32)
	if err != nil {
		return 0, err
	}
	return math.Float32bits(float32(v)), nil
}

func (p *parser) instruction() error {
	p.skipSpace()
	name := p.word()
	upper := strings.ToUpper(name)
	var inst Instruction
	op, ok := opcodesByName[upper]
	if !ok {
		// The longest opcode that the name begins with, before its
		// modifiers.
		for i := len(upper) - 1; i > 0 && !ok; i-- {
			op, ok = opcodesByName[upper[:i]]
			if ok {
				name = name[:i]
			}
		}
		if !ok {
			return p.fail("unknown opcode %q", p.word())
		}
	}
	p.pos += len(name)
	inst.Opcode = op
	for {
		switch {
		case p.matchSuffix("_SAT"):
			inst.Saturate = true
			continue
		case p.matchSuffix("_PRECISE"):
			inst.Precise = true
			continue
		}
		break
	}
	info, _ := op.Info()
	for i := 0; i < info.NumDst; i++ {
		if i > 0 {
			if err := p.expect(','); err != nil {
				return err
			}
		}
		if !p.skipSpace() {
			return p.fail("syntax error")
		}
		dst, err := p.dst()
		if err != nil {
			return err
		}
		inst.Dst = append(inst.Dst, dst)
	}
	for i := 0; i < info.NumSrc; i++ {
		if i > 0 || info.NumDst > 0 {
			if err := p.expect(','); err != nil {
				return err
			}
		}
		if !p.skipSpace() {
			return p.fail("syntax error")
		}
		src, err := p.src()
		if err != nil {
			return err
		}
		inst.Src = append(inst.Src, src)
	}
	// A texture's target, which the SAMPLE opcodes, of a sampler view's,
	// lack, and its offsets.
	isSample := op >= OpSAMPLE && op <= OpGATHER4
	if info.IsTex && !isSample {
		if err := p.expect(','); err != nil {
			return err
		}
		p.skipBlanks()
		t, ok := p.matchEnum(textureNames)
		if !ok {
			return p.fail("expected a texture target")
		}
		inst.Texture = TextureTarget(t)
	}
	if info.IsTex || isSample {
		for {
			save := p.pos
			p.skipBlanks()
			if p.peek() != ',' {
				p.pos = save
				break
			}
			p.pos++
			p.skipBlanks()
			f, ok := p.matchEnum(fileNames)
			if !ok {
				p.pos = save
				break
			}
			off := TexOffset{File: File(f)}
			if err := p.expect('['); err != nil {
				return err
			}
			idx, err := p.index()
			if err != nil {
				return err
			}
			off.Index = idx
			if err := p.expect(']'); err != nil {
				return err
			}
			if err := p.expect('.'); err != nil {
				return err
			}
			for i := 0; i < 3; i++ {
				c, ok := p.swizzleChannel()
				if !ok {
					return p.fail("expected a channel")
				}
				off.Swizzle[i] = c
			}
			inst.TexOffset = append(inst.TexOffset, off)
		}
	}
	// Memory qualifiers, and the texture kind and format of an image.
	for op.accessesMemory() {
		save := p.pos
		p.skipBlanks()
		if p.peek() != ',' {
			p.pos = save
			break
		}
		p.pos++
		p.skipBlanks()
		if q, ok := p.matchEnum(memoryQualifierNames); ok {
			inst.Memory |= 1 << q
			continue
		}
		if t, ok := p.matchEnum(textureNames); ok {
			inst.MemoryTexture = TextureTarget(t)
			continue
		}
		if w := p.word(); strings.HasPrefix(strings.ToUpper(w), "PIPE_FORMAT_") {
			inst.MemoryFormat = w
			p.pos += len(w)
			continue
		}
		p.pos = save
		break
	}
	// A branch's label.
	save := p.pos
	p.skipBlanks()
	if info.IsBranch && p.peek() == ':' {
		p.pos++
		p.skipBlanks()
		l, ok := p.uint()
		if !ok {
			return p.fail("expected a label")
		}
		inst.HasLabel = true
		inst.Label = int(l)
	} else {
		p.pos = save
	}
	p.s.Instructions = append(p.s.Instructions, inst)
	return nil
}

// accessesMemory reports whether op takes memory qualifiers: Mesa's LOAD
// through ATOMIMAX and RESQ, and the atomics that Mesa added after them.
func (op Opcode) accessesMemory() bool {
	switch {
	case op >= OpLOAD && op <= OpATOMIMAX, op == OpRESQ, op == OpATOMFADD,
		op == OpATOMINC_WRAP, op == OpATOMDEC_WRAP:
		return true
	}
	return false
}

// matchSuffix consumes s, an opcode's modifier, of any case.
func (p *parser) matchSuffix(s string) bool {
	if len(p.text)-p.pos >= len(s) && strings.EqualFold(p.text[p.pos:p.pos+len(s)], s) {
		p.pos += len(s)
		return true
	}
	return false
}

// indirect parses an indirect index, inside a register's brackets: a
// register's channel, plus an index, and an array's ID.
func (p *parser) indirect() (*Indirect, int, error) {
	f, err := p.file()
	if err != nil {
		return nil, 0, err
	}
	ind := &Indirect{File: f}
	if err := p.expect('['); err != nil {
		return nil, 0, err
	}
	if ind.Index, err = p.index(); err != nil {
		return nil, 0, err
	}
	if err := p.expect(']'); err != nil {
		return nil, 0, err
	}
	if err := p.expect('.'); err != nil {
		return nil, 0, err
	}
	c, ok := p.swizzleChannel()
	if !ok {
		return nil, 0, p.fail("expected a channel")
	}
	ind.Swizzle = c
	p.skipBlanks()
	offset := 0
	if c := p.peek(); c == '+' || c == '-' {
		if offset, err = p.index(); err != nil {
			return nil, 0, err
		}
	}
	if err := p.expect(']'); err != nil {
		return nil, 0, err
	}
	save := p.pos
	p.skipBlanks()
	if p.peek() == '(' {
		p.pos++
		if ind.ArrayID, err = p.index(); err != nil {
			return nil, 0, err
		}
		if err := p.expect(')'); err != nil {
			return nil, 0, err
		}
	} else {
		p.pos = save
	}
	return ind, offset, nil
}

// register parses a register: its file, and one or two bracketed indexes,
// of which the first of two is its dimension's.
func (p *parser) register() (Register, error) {
	var r Register
	var err error
	if r.File, err = p.file(); err != nil {
		return r, err
	}
	type bracket struct {
		index    int
		indirect *Indirect
	}
	var brackets []bracket
	for len(brackets) < 2 {
		save := p.pos
		p.skipBlanks()
		if p.peek() != '[' {
			p.pos = save
			break
		}
		p.pos++
		p.skipBlanks()
		var b bracket
		if c := p.peek(); c >= '0' && c <= '9' || c == '-' || c == '+' {
			if b.index, err = p.index(); err != nil {
				return r, err
			}
			if err := p.expect(']'); err != nil {
				return r, err
			}
		} else if b.indirect, b.index, err = p.indirect(); err != nil {
			return r, err
		}
		brackets = append(brackets, b)
	}
	switch len(brackets) {
	case 1:
		r.Index, r.Indirect = brackets[0].index, brackets[0].indirect
	case 2:
		r.Dimension = true
		r.DimIndex, r.DimIndirect = brackets[0].index, brackets[0].indirect
		r.Index, r.Indirect = brackets[1].index, brackets[1].indirect
	default:
		return r, p.fail("expected a register's index")
	}
	return r, nil
}

func (p *parser) dst() (DstRegister, error) {
	r, err := p.register()
	if err != nil {
		return DstRegister{}, err
	}
	mask, err := p.writeMask()
	if err != nil {
		return DstRegister{}, err
	}
	return DstRegister{Register: r, WriteMask: mask}, nil
}

func (p *parser) src() (SrcRegister, error) {
	var s SrcRegister
	if p.peek() == '-' {
		s.Negate = true
		p.pos++
		p.skipBlanks()
	}
	if p.peek() == '|' {
		s.Absolute = true
		p.pos++
		p.skipBlanks()
	}
	r, err := p.register()
	if err != nil {
		return s, err
	}
	s.Register = r
	s.Swizzle = identitySwizzle
	save := p.pos
	p.skipBlanks()
	if p.peek() == '.' {
		p.pos++
		p.skipBlanks()
		// One to four channels; the last fills the rest, as in Mesa.
		n := 0
		for n < 4 {
			c, ok := p.swizzleChannel()
			if !ok {
				break
			}
			s.Swizzle[n] = c
			n++
		}
		if n == 0 {
			return s, p.fail("expected a swizzle")
		}
		for i := n; i < 4; i++ {
			s.Swizzle[i] = s.Swizzle[n-1]
		}
	} else {
		p.pos = save
	}
	if s.Absolute {
		if err := p.expect('|'); err != nil {
			return s, err
		}
	}
	return s, nil
}
