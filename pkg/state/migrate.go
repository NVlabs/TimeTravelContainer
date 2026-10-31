// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

// Loading what older builds saved.
//
// An image records, for each savable type in it, the type's name and the
// names of the fields it saved. A build loads a saved struct into a type of
// the same name with the same fields. Where an older build saved a type
// otherwise, because TTC or upstream gVisor changed it since, the type says
// how it loads from that older shape: a Migration registered for exactly the
// fields that the older build saved, which accounts for every field that
// differs. Nothing differs silently. A field that this build added gets the
// value that its migration gives it, a field that it removed is dropped only
// where its migration says so, and an older shape that no migration covers is
// refused, naming the type and the fields that differ.

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/state/wire"
)

// Migration says how a savable type loads from the images of a build that
// saved it with other fields, or under another name.
type Migration struct {
	// Type is the type's name in this build: its StateTypeName.
	Type string

	// OldName is the type's name in the older build, if it differs.
	OldName string

	// Fields are the fields that the older build saved of the type, in any
	// order.
	Fields []string

	// Renamed maps a field of the older build to the field of this build
	// that holds its value, which must be saved alike.
	Renamed map[string]string

	// Dropped are fields of the older build that this one does without.
	// What only they referenced is discarded.
	Dropped []string

	// Zero are fields of this build that the older one did not save, and
	// whose zero value is right for what it saved. A field of an empty
	// struct type needs no mention: it has no other value.
	Zero []string

	// Set are the other fields of this build that the older one did not
	// save, or saved as another type: Fix or FixAfterLoad sets them.
	Set []string

	// Old are fields of the older build whose values the fixes need, each
	// with a pointer to a value of the type that the older build saved it
	// as. A field that this build saves under the same name, as another
	// type, must be among them, and in Set. The older build's value of each
	// is loaded into a new value of its type, and the fixes get pointers to
	// them by name.
	Old map[string]any

	// Fix, if set, is called with a pointer to each object of the type that
	// loads from such an image, once the objects that it waits for have
	// loaded, and before its own afterLoad, to set the fields in Set from
	// its own other fields and old values.
	Fix func(obj any, old map[string]any)

	// FixAfterLoad, if set, is called with a pointer to each object of the
	// type that loads from such an image once the whole image has loaded,
	// every afterLoad included, to set the fields in Set from other objects.
	FixAfterLoad func(obj any, old map[string]any)

	// Refuse, if set, says why this build does not load the older shape at
	// all, as of a type that TTC never saves: an image that holds one is
	// refused, with this reason. A type that this build does not have is
	// refused by a Migration with only OldName and Refuse.
	Refuse string
}

// migrations is written only by RegisterMigration, from init functions, and
// read without its lock once they have run, while images load.
var migrations struct {
	mu sync.Mutex

	// byType maps a type's name in this build to its migrations.
	byType map[string][]*Migration

	// renamed maps a type's name in an older build to its name in this
	// one.
	renamed map[string]string

	// refused maps the name of a type that this build does not have to why
	// it refuses images that hold one.
	refused map[string]string
}

// RegisterMigration registers m. A package registers the migrations of its
// types from an init function, and only from one.
func RegisterMigration(m Migration) {
	migrations.mu.Lock()
	defer migrations.mu.Unlock()
	if migrations.byType == nil {
		migrations.byType = make(map[string][]*Migration)
		migrations.renamed = make(map[string]string)
		migrations.refused = make(map[string]string)
	}
	if m.Type == "" && m.OldName != "" && m.Refuse != "" {
		migrations.refused[m.OldName] = m.Refuse
		return
	}
	if m.Type == "" || len(m.Fields) == 0 && m.OldName == "" {
		panic(fmt.Sprintf("state: a migration of %q must name the older build's fields or name", m.Type))
	}
	migrations.byType[m.Type] = append(migrations.byType[m.Type], &m)
	if m.OldName != "" {
		if other, ok := migrations.renamed[m.OldName]; ok && other != m.Type {
			panic(fmt.Sprintf("state: type %q renamed both to %q and to %q", m.OldName, other, m.Type))
		}
		migrations.renamed[m.OldName] = m.Type
	}
}

// currentName returns the name in this build of a type that an older build
// saved as name.
func currentName(name string) string {
	if n, ok := migrations.renamed[name]; ok {
		return n
	}
	return name
}

// migrationFor returns the migration of the type that this build names name
// from images that saved it as savedName with the fields saved, or nil.
func migrationFor(name, savedName string, saved []string) *Migration {
	for _, m := range migrations.byType[name] {
		if savedName != name && m.OldName != savedName {
			continue
		}
		if len(m.Fields) == 0 || sameFields(m.Fields, saved) {
			return m
		}
	}
	return nil
}

// oldField is a field of an older build's image whose value a migration's
// fixes need: its index among the image's fields, and its name.
type oldField struct {
	index int
	name  string
}

// coerce sets obj from the encoded value w, which an older build saved as a
// type that obj no longer has, where only one meaning is possible, and
// returns whether it did:
//
//   - A time saved as an integer is its UnixNano, as savers of times were
//     before pkg/state saved times itself; they loaded it with
//     time.Unix(0, n).
//   - A bool saved where this build keeps an atomic one (a struct with a
//     Store(bool) method).
//   - A struct saved where this build keeps a pointer to one: the field held
//     the value before.
func (ds *decodeState) coerce(ods *objectDecodeState, obj reflect.Value, w wire.Object) bool {
	switch x := w.(type) {
	case wire.Int:
		if obj.Type() == timeType {
			obj.Set(reflect.ValueOf(time.Unix(0, int64(x))))
			return true
		}
	case wire.Bool:
		if obj.Kind() == reflect.Struct && obj.CanAddr() {
			if b, ok := obj.Addr().Interface().(interface{ Store(bool) }); ok {
				b.Store(bool(x))
				return true
			}
		}
	case *wire.Struct:
		if obj.Kind() == reflect.Pointer && x.TypeID != 0 {
			p := reflect.New(obj.Type().Elem())
			ds.decodeStruct(ods, p.Elem(), x)
			obj.Set(p)
			return true
		}
	}
	return false
}

var timeType = reflect.TypeFor[time.Time]()

// implicitMigration returns the migration of typ, which this build saves with
// fields as name, from an image that saved it with the fields saved, if it
// needs none from its package: if every field that differs is one that this
// build added of an empty struct type, which has no value to choose. It
// returns nil otherwise.
func implicitMigration(typ reflect.Type, name string, fields, saved []string) *Migration {
	for _, f := range saved {
		if !slices.Contains(fields, f) {
			return nil
		}
	}
	for _, f := range fields {
		if !slices.Contains(saved, f) && !emptyStructField(typ, f) {
			return nil
		}
	}
	return &Migration{Type: name, Fields: saved}
}

// sameFields returns whether a and b hold the same field names.
func sameFields(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// SchemaError is the error of loading an image whose build saved a type
// otherwise than this build does, from which this build has no migration.
type SchemaError struct {
	// Type is the type's name in the image.
	Type string

	// Missing are fields of this build that the image lacks.
	Missing []string

	// Extra are fields of the image that this build lacks.
	Extra []string

	// Reason, if set, is why this build refuses the type's older shape.
	Reason string
}

// Error implements error.Error.
func (e *SchemaError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("an older build saved this image with a %s, which this build does not load: %s", e.Type, e.Reason)
	}
	var parts []string
	if len(e.Extra) != 0 {
		parts = append(parts, fmt.Sprintf("fields %s that this build does not have", strings.Join(e.Extra, ", ")))
	}
	if len(e.Missing) != 0 {
		parts = append(parts, fmt.Sprintf("none of its fields %s", strings.Join(e.Missing, ", ")))
	}
	return fmt.Sprintf("an older build saved this image, with %s of %s, and this build has no migration from that", strings.Join(parts, " and "), e.Type)
}

// IsSchemaError returns whether err is, or wraps, a SchemaError.
func IsSchemaError(err error) bool {
	var se *SchemaError
	return errors.As(err, &se)
}

// newSchemaError returns the SchemaError of type name, which this build saves
// with fields, from an image that saved it with saved.
func newSchemaError(name string, fields, saved []string) *SchemaError {
	e := &SchemaError{Type: name}
	for _, f := range fields {
		if !slices.Contains(saved, f) {
			e.Missing = append(e.Missing, f)
		}
	}
	for _, f := range saved {
		if !slices.Contains(fields, f) {
			e.Extra = append(e.Extra, f)
		}
	}
	return e
}

// reconcileMigrated returns the field order of typ, which this build saves
// with fields as name, loading from saved under m: for each field, the index
// of the saved field that holds its value, or -1 if the image lacks it. It
// also returns the indices of the saved fields that m drops, and of those
// whose older values it needs. It fails if m does not account for every field
// that differs.
func reconcileMigrated(m *Migration, typ reflect.Type, name string, fields, saved []string) (order, dropped []int, olds []oldField) {
	bad := func(format string, v ...any) {
		Failf("state: the migration of %s from its fields %v %s", name, saved, fmt.Sprintf(format, v...))
	}
	local := make(map[string]bool, len(fields))
	for _, f := range fields {
		local[f] = true
	}
	// from maps each local field to the index of the saved field that holds
	// its value.
	from := make(map[string]int, len(fields))
	for i, f := range saved {
		_, old := m.Old[f]
		switch to, renamed := m.Renamed[f]; {
		case old:
			olds = append(olds, oldField{index: i, name: f})
		case renamed:
			if !local[to] {
				bad("renames %s to %s, which this build does not save", f, to)
			}
			from[to] = i
		case slices.Contains(m.Dropped, f):
			if local[f] {
				bad("drops %s, which this build saves", f)
			}
			dropped = append(dropped, i)
		case local[f]:
			from[f] = i
		default:
			bad("does not say what becomes of %s", f)
		}
	}
	order = make([]int, len(fields))
	for i, f := range fields {
		if j, ok := from[f]; ok {
			order[i] = j
			continue
		}
		order[i] = -1
		if !slices.Contains(m.Zero, f) && !slices.Contains(m.Set, f) && !emptyStructField(typ, f) {
			bad("does not say what %s, which the image lacks, should be", f)
		}
	}
	for f := range m.Old {
		if !slices.Contains(saved, f) {
			bad("needs the value of %s, which the image lacks", f)
		}
	}
	return order, dropped, olds
}

// emptyStructField returns whether field name of typ is of an empty struct
// type, which has a single value.
func emptyStructField(typ reflect.Type, name string) bool {
	if typ.Kind() != reflect.Struct {
		return false
	}
	f, ok := typ.FieldByName(name)
	return ok && f.Type.Kind() == reflect.Struct && f.Type.Size() == 0
}

// refusal returns why this build refuses the type that an older build saved
// as name, if it does.
func refusal(name string) string {
	return migrations.refused[name]
}

// refsIn appends to ids the objects that w references.
func refsIn(w wire.Object, ids []objectID) []objectID {
	switch v := w.(type) {
	case *wire.Ref:
		if v.Root != 0 {
			ids = append(ids, objectID(v.Root))
		}
	case *wire.Slice:
		if v.Ref.Root != 0 {
			ids = append(ids, objectID(v.Ref.Root))
		}
	case *wire.Struct:
		for i := 0; i < v.Fields(); i++ {
			ids = refsIn(*v.Field(i), ids)
		}
	case *wire.Array:
		for _, o := range v.Contents {
			ids = refsIn(o, ids)
		}
	case *wire.Map:
		for i := range v.Keys {
			ids = refsIn(v.Keys[i], ids)
			ids = refsIn(v.Values[i], ids)
		}
	case *wire.Interface:
		ids = refsIn(v.Value, ids)
	}
	return ids
}

// SchemaType is what images hold of a savable type: the fields it saves, and
// the type that each is saved as.
type SchemaType struct {
	Fields []string `json:"fields"`
	Types  []string `json:"types"`
}

// Schema returns, by name, the SchemaType of every type that this build
// registered: the shape of the images that it saves.
func Schema() map[string]SchemaType {
	schema := make(map[string]SchemaType, len(globalTypeDatabase))
	for name, typ := range globalTypeDatabase {
		t, ok := reflect.New(typ).Interface().(Type)
		if !ok {
			continue
		}
		fields := t.StateFields()
		st := SchemaType{Fields: fields, Types: make([]string, len(fields))}
		for i, f := range fields {
			st.Types[i] = savedType(typ, f)
		}
		schema[name] = st
	}
	return schema
}

// savedType returns the type that field name of typ is saved as: the one its
// state tag converts it to, or its own.
func savedType(typ reflect.Type, name string) string {
	if typ.Kind() != reflect.Struct {
		return ""
	}
	f, ok := typ.FieldByName(name)
	if !ok {
		return "?"
	}
	if tag := f.Tag.Get("state"); strings.HasPrefix(tag, ".(") && strings.HasSuffix(tag, ")") {
		return tag[2 : len(tag)-1]
	}
	if f.Type.Kind() == reflect.Interface {
		// What an interface holds is saved with its own type.
		return "interface"
	}
	return f.Type.String()
}

// CheckMigration returns an error unless this build loads the type that an
// older build saved as name, with the fields and types of saved, from that
// build's images: with the same fields saved as the same types, or with a
// migration that accounts for every difference, or coercions where only one
// meaning is possible.
func CheckMigration(name string, saved SchemaType) error {
	if refusal(name) != "" {
		return nil
	}
	cur := currentName(name)
	typ, ok := globalTypeDatabase[cur]
	if !ok {
		return fmt.Errorf("an older build saved %s, which this build does not have", name)
	}
	t, ok := reflect.New(typ).Interface().(Type)
	if !ok {
		return fmt.Errorf("%s is not a savable type", cur)
	}
	fields := t.StateFields()
	var m *Migration
	if cur != name || !sameFields(fields, saved.Fields) {
		m = migrationFor(cur, name, saved.Fields)
		if m == nil && cur == name {
			m = implicitMigration(typ, cur, fields, saved.Fields)
		}
		if m == nil {
			return newSchemaError(name, fields, saved.Fields)
		}
		if m.Refuse != "" {
			return nil
		}
		if err := safely(func() {
			_, _, _ = reconcileMigrated(m, typ, cur, fields, saved.Fields)
		}); err != nil {
			return err
		}
	}
	if len(saved.Types) != len(saved.Fields) {
		return nil
	}
	for i, f := range saved.Fields {
		local := f
		if m != nil {
			if _, ok := m.Old[f]; ok {
				continue
			}
			if to, ok := m.Renamed[f]; ok {
				local = to
			}
		}
		if !slices.Contains(fields, local) {
			continue
		}
		was, is := renamedTypes(unqualified(saved.Types[i])), unqualified(savedType(typ, local))
		if was == is || coercible(was, is) {
			continue
		}
		return fmt.Errorf("%s saves %s as %s, which an older build saved as %s, and no migration converts it", cur, local, is, was)
	}
	return nil
}

var qualifier = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*\.`)

// unqualified returns the type expression typ without package qualifiers.
func unqualified(typ string) string {
	return qualifier.ReplaceAllString(typ, "")
}

// renamedTypes returns typ, an unqualified type expression of an older build,
// with the names of the types that this build renamed replaced by theirs.
func renamedTypes(typ string) string {
	for old, cur := range migrations.renamed {
		o, c := old[strings.LastIndex(old, ".")+1:], cur[strings.LastIndex(cur, ".")+1:]
		typ = regexp.MustCompile(`\b`+regexp.QuoteMeta(o)+`\b`).ReplaceAllString(typ, c)
	}
	return typ
}

// coercible returns whether a field that an older build saved as type was
// loads as type is through one of the decoder's coercions (see coerce).
func coercible(was, is string) bool {
	switch {
	case was == "int64" && is == "Time":
		return true
	case was == "bool" && is == "Bool":
		return true
	case "*"+was == is:
		return true
	}
	return false
}
