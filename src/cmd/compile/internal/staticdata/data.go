// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package staticdata

import (
	"crypto/sha256"
	"fmt"
	"go/constant"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"strconv"
	"sync"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/src"
)

// InitAddr writes the static address of a to n. a must be an ONAME.
// Neither n nor a is modified.
func InitAddr(n *ir.Name, noff int64, a *ir.Name, aoff int64) {
	if n.Op() != ir.ONAME {
		base.Fatalf("addrsym n op %v", n.Op())
	}
	if n.Sym() == nil {
		base.Fatalf("addrsym nil n sym")
	}
	if a.Op() != ir.ONAME {
		base.Fatalf("addrsym a op %v", a.Op())
	}
	s := n.Sym().Linksym()
	s.WriteAddr(base.Ctxt, noff, types.PtrSize, a.Sym().Linksym(), aoff)
}

// InitFunc writes the static address of f to n. f must be a global function.
// Neither n nor f is modified.
func InitFunc(n *ir.Name, noff int64, f *ir.Name) {
	if n.Op() != ir.ONAME {
		base.Fatalf("pfuncsym n op %v", n.Op())
	}
	if n.Sym() == nil {
		base.Fatalf("pfuncsym nil n sym")
	}
	if f.Class_ != ir.PFUNC {
		base.Fatalf("pfuncsym class not PFUNC %d", f.Class_)
	}
	s := n.Sym().Linksym()
	s.WriteAddr(base.Ctxt, noff, types.PtrSize, FuncSym(f.Sym()).Linksym(), 0)
}

// InitSlice writes a static slice symbol {&arr, lencap, lencap} to n+noff.
// InitSlice does not modify n.
func InitSlice(n *ir.Name, noff int64, arr *ir.Name, lencap int64) {
	s := n.Sym().Linksym()
	if arr.Op() != ir.ONAME {
		base.Fatalf("slicesym non-name arr %v", arr)
	}
	s.WriteAddr(base.Ctxt, noff, types.PtrSize, arr.Sym().Linksym(), 0)
	s.WriteInt(base.Ctxt, noff+types.SliceLenOffset, types.PtrSize, lencap)
	s.WriteInt(base.Ctxt, noff+types.SliceCapOffset, types.PtrSize, lencap)
}

func InitSliceBytes(nam *ir.Name, off int64, s string) {
	if nam.Op() != ir.ONAME {
		base.Fatalf("slicebytes %v", nam)
	}
	InitSlice(nam, off, slicedata(nam.Pos(), s), int64(len(s)))
}

const (
	stringSymPrefix  = "go.string."
	stringSymPattern = ".gostring.%d.%x"
)

// StringSym returns a symbol containing the string s.
// The symbol contains the string data, not a string header.
func StringSym(pos src.XPos, s string) (data *obj.LSym) {
	var symname string
	if len(s) > 100 {
		// Huge strings are hashed to avoid long names in object files.
		// Indulge in some paranoia by writing the length of s, too,
		// as protection against length extension attacks.
		// Same pattern is known to fileStringSym below.
		h := sha256.New()
		io.WriteString(h, s)
		symname = fmt.Sprintf(stringSymPattern, len(s), h.Sum(nil))
	} else {
		// Small strings get named directly by their contents.
		symname = strconv.Quote(s)
	}

	symdata := base.Ctxt.Lookup(stringSymPrefix + symname)
	if !symdata.OnList() {
		off := dstringdata(symdata, 0, s, pos, "string")
		objw.Global(symdata, int32(off), obj.DUPOK|obj.RODATA|obj.LOCAL)
		symdata.Set(obj.AttrContentAddressable, true)
	}

	return symdata
}

// fileStringSym returns a symbol for the contents and the size of file.
// If readonly is true, the symbol shares storage with any literal string
// or other file with the same content and is placed in a read-only section.
// If readonly is false, the symbol is a read-write copy separate from any other,
// for use as the backing store of a []byte.
// The content hash of file is copied into hash. (If hash is nil, nothing is copied.)
// The returned symbol contains the data itself, not a string header.
func fileStringSym(pos src.XPos, file string, readonly bool, hash []byte) (*obj.LSym, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("not a regular file")
	}
	size := info.Size()
	if size <= 1*1024 {
		data, err := ioutil.ReadAll(f)
		if err != nil {
			return nil, 0, err
		}
		if int64(len(data)) != size {
			return nil, 0, fmt.Errorf("file changed between reads")
		}
		var sym *obj.LSym
		if readonly {
			sym = StringSym(pos, string(data))
		} else {
			sym = slicedata(pos, string(data)).Sym().Linksym()
		}
		if len(hash) > 0 {
			sum := sha256.Sum256(data)
			copy(hash, sum[:])
		}
		return sym, size, nil
	}
	if size > 2e9 {
		// ggloblsym takes an int32,
		// and probably the rest of the toolchain
		// can't handle such big symbols either.
		// See golang.org/issue/9862.
		return nil, 0, fmt.Errorf("file too large")
	}

	// File is too big to read and keep in memory.
	// Compute hash if needed for read-only content hashing or if the caller wants it.
	var sum []byte
	if readonly || len(hash) > 0 {
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return nil, 0, err
		}
		if n != size {
			return nil, 0, fmt.Errorf("file changed between reads")
		}
		sum = h.Sum(nil)
		copy(hash, sum)
	}

	var symdata *obj.LSym
	if readonly {
		symname := fmt.Sprintf(stringSymPattern, size, sum)
		symdata = base.Ctxt.Lookup(stringSymPrefix + symname)
		if !symdata.OnList() {
			info := symdata.NewFileInfo()
			info.Name = file
			info.Size = size
			objw.Global(symdata, int32(size), obj.DUPOK|obj.RODATA|obj.LOCAL)
			// Note: AttrContentAddressable cannot be set here,
			// because the content-addressable-handling code
			// does not know about file symbols.
		}
	} else {
		// Emit a zero-length data symbol
		// and then fix up length and content to use file.
		symdata = slicedata(pos, "").Sym().Linksym()
		symdata.Size = size
		symdata.Type = objabi.SNOPTRDATA
		info := symdata.NewFileInfo()
		info.Name = file
		info.Size = size
	}

	return symdata, size, nil
}

var slicedataGen int

func slicedata(pos src.XPos, s string) *ir.Name {
	slicedataGen++
	symname := fmt.Sprintf(".gobytes.%d", slicedataGen)
	sym := types.LocalPkg.Lookup(symname)
	symnode := typecheck.NewName(sym)
	sym.Def = symnode

	lsym := sym.Linksym()
	off := dstringdata(lsym, 0, s, pos, "slice")
	objw.Global(lsym, int32(off), obj.NOPTR|obj.LOCAL)

	return symnode
}

func dstringdata(s *obj.LSym, off int, t string, pos src.XPos, what string) int {
	// Objects that are too large will cause the data section to overflow right away,
	// causing a cryptic error message by the linker. Check for oversize objects here
	// and provide a useful error message instead.
	if int64(len(t)) > 2e9 {
		base.ErrorfAt(pos, "%v with length %v is too big", what, len(t))
		return 0
	}

	s.WriteString(base.Ctxt, int64(off), len(t), t)
	return off + len(t)
}

var (
	funcsymsmu sync.Mutex // protects funcsyms and associated package lookups (see func funcsym)
	funcsyms   []*types.Sym
)

// FuncSym returns s·f.
func FuncSym(s *types.Sym) *types.Sym {
	// funcsymsmu here serves to protect not just mutations of funcsyms (below),
	// but also the package lookup of the func sym name,
	// since this function gets called concurrently from the backend.
	// There are no other concurrent package lookups in the backend,
	// except for the types package, which is protected separately.
	// Reusing funcsymsmu to also cover this package lookup
	// avoids a general, broader, expensive package lookup mutex.
	// Note makefuncsym also does package look-up of func sym names,
	// but that it is only called serially, from the front end.
	funcsymsmu.Lock()
	sf, existed := s.Pkg.LookupOK(ir.FuncSymName(s))
	// Don't export s·f when compiling for dynamic linking.
	// When dynamically linking, the necessary function
	// symbols will be created explicitly with makefuncsym.
	// See the makefuncsym comment for details.
	if !base.Ctxt.Flag_dynlink && !existed {
		funcsyms = append(funcsyms, s)
	}
	funcsymsmu.Unlock()
	return sf
}

// NeedFuncSym ensures that s·f is exported.
// It is only used with -dynlink.
// When not compiling for dynamic linking,
// the funcsyms are created as needed by
// the packages that use them.
// Normally we emit the s·f stubs as DUPOK syms,
// but DUPOK doesn't work across shared library boundaries.
// So instead, when dynamic linking, we only create
// the s·f stubs in s's package.
func NeedFuncSym(s *types.Sym) {
	if !base.Ctxt.Flag_dynlink {
		base.Fatalf("makefuncsym dynlink")
	}
	if s.IsBlank() {
		return
	}
	if base.Flag.CompilingRuntime && (s.Name == "getg" || s.Name == "getclosureptr" || s.Name == "getcallerpc" || s.Name == "getcallersp") {
		// runtime.getg(), getclosureptr(), getcallerpc(), and
		// getcallersp() are not real functions and so do not
		// get funcsyms.
		return
	}
	if _, existed := s.Pkg.LookupOK(ir.FuncSymName(s)); !existed {
		funcsyms = append(funcsyms, s)
	}
}

func WriteFuncSyms() {
	sort.Slice(funcsyms, func(i, j int) bool {
		return funcsyms[i].LinksymName() < funcsyms[j].LinksymName()
	})
	for _, s := range funcsyms {
		sf := s.Pkg.Lookup(ir.FuncSymName(s)).Linksym()
		objw.SymPtr(sf, 0, s.Linksym(), 0)
		objw.Global(sf, int32(types.PtrSize), obj.DUPOK|obj.RODATA)
	}
}

// InitConst writes the static literal c to n.
// Neither n nor c is modified.
func InitConst(n *ir.Name, noff int64, c ir.Node, wid int) {
	if n.Op() != ir.ONAME {
		base.Fatalf("litsym n op %v", n.Op())
	}
	if n.Sym() == nil {
		base.Fatalf("litsym nil n sym")
	}
	if c.Op() == ir.ONIL {
		return
	}
	if c.Op() != ir.OLITERAL {
		base.Fatalf("litsym c op %v", c.Op())
	}
	s := n.Sym().Linksym()
	switch u := c.Val(); u.Kind() {
	case constant.Bool:
		i := int64(obj.Bool2int(constant.BoolVal(u)))
		s.WriteInt(base.Ctxt, noff, wid, i)

	case constant.Int:
		s.WriteInt(base.Ctxt, noff, wid, ir.IntVal(c.Type(), u))

	case constant.Float:
		f, _ := constant.Float64Val(u)
		switch c.Type().Kind() {
		case types.TFLOAT32:
			s.WriteFloat32(base.Ctxt, noff, float32(f))
		case types.TFLOAT64:
			s.WriteFloat64(base.Ctxt, noff, f)
		}

	case constant.Complex:
		re, _ := constant.Float64Val(constant.Real(u))
		im, _ := constant.Float64Val(constant.Imag(u))
		switch c.Type().Kind() {
		case types.TCOMPLEX64:
			s.WriteFloat32(base.Ctxt, noff, float32(re))
			s.WriteFloat32(base.Ctxt, noff+4, float32(im))
		case types.TCOMPLEX128:
			s.WriteFloat64(base.Ctxt, noff, re)
			s.WriteFloat64(base.Ctxt, noff+8, im)
		}

	case constant.String:
		i := constant.StringVal(u)
		symdata := StringSym(n.Pos(), i)
		s.WriteAddr(base.Ctxt, noff, types.PtrSize, symdata, 0)
		s.WriteInt(base.Ctxt, noff+int64(types.PtrSize), types.PtrSize, int64(len(i)))

	default:
		base.Fatalf("litsym unhandled OLITERAL %v", c)
	}
}
