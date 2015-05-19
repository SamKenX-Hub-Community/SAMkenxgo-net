// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// TODO(nigeltao): eliminate the concept of a configurable PropSystem, and the
// MemPS implementation. Properties are now the responsibility of a File
// implementation, not a PropSystem implementation.

// PropSystem manages the properties of named resources. It allows finding
// and setting properties as defined in RFC 4918.
//
// The elements in a resource name are separated by slash ('/', U+002F)
// characters, regardless of host operating system convention.
type PropSystem interface {
	// Find returns the status of properties named propnames for resource name.
	//
	// Each Propstat must have a unique status and each property name must
	// only be part of one Propstat element.
	Find(name string, propnames []xml.Name) ([]Propstat, error)

	// TODO(nigeltao) merge Find and Allprop?

	// Allprop returns the properties defined for resource name and the
	// properties named in include. The returned Propstats are handled
	// as in Find.
	//
	// Note that RFC 4918 defines 'allprop' to return the DAV: properties
	// defined within the RFC plus dead properties. Other live properties
	// should only be returned if they are named in 'include'.
	//
	// See http://www.webdav.org/specs/rfc4918.html#METHOD_PROPFIND
	Allprop(name string, include []xml.Name) ([]Propstat, error)

	// Propnames returns the property names defined for resource name.
	Propnames(name string) ([]xml.Name, error)

	// Patch patches the properties of resource name.
	//
	// If all patches can be applied without conflict, Patch returns a slice
	// of length one and a Propstat element of status 200, naming all patched
	// properties. In case of conflict, Patch returns an arbitrary long slice
	// and no Propstat element must have status 200. In either case, properties
	// in Propstat must not have values.
	//
	// Note that the WebDAV RFC requires either all patches to succeed or none.
	Patch(name string, patches []Proppatch) ([]Propstat, error)
}

// Proppatch describes a property update instruction as defined in RFC 4918.
// See http://www.webdav.org/specs/rfc4918.html#METHOD_PROPPATCH
type Proppatch struct {
	// Remove specifies whether this patch removes properties. If it does not
	// remove them, it sets them.
	Remove bool
	// Props contains the properties to be set or removed.
	Props []Property
}

// Propstat describes a XML propstat element as defined in RFC 4918.
// See http://www.webdav.org/specs/rfc4918.html#ELEMENT_propstat
type Propstat struct {
	// Props contains the properties for which Status applies.
	Props []Property

	// Status defines the HTTP status code of the properties in Prop.
	// Allowed values include, but are not limited to the WebDAV status
	// code extensions for HTTP/1.1.
	// http://www.webdav.org/specs/rfc4918.html#status.code.extensions.to.http11
	Status int

	// XMLError contains the XML representation of the optional error element.
	// XML content within this field must not rely on any predefined
	// namespace declarations or prefixes. If empty, the XML error element
	// is omitted.
	XMLError string

	// ResponseDescription contains the contents of the optional
	// responsedescription field. If empty, the XML element is omitted.
	ResponseDescription string
}

// makePropstats returns a slice containing those of x and y whose Props slice
// is non-empty. If both are empty, it returns a slice containing an otherwise
// zero Propstat whose HTTP status code is 200 OK.
func makePropstats(x, y Propstat) []Propstat {
	pstats := make([]Propstat, 0, 2)
	if len(x.Props) != 0 {
		pstats = append(pstats, x)
	}
	if len(y.Props) != 0 {
		pstats = append(pstats, y)
	}
	if len(pstats) == 0 {
		pstats = append(pstats, Propstat{
			Status: http.StatusOK,
		})
	}
	return pstats
}

// DeadPropsHolder holds the dead properties of a resource.
//
// Dead properties are those properties that are explicitly defined. In
// comparison, live properties, such as DAV:getcontentlength, are implicitly
// defined by the underlying resource, and cannot be explicitly overridden or
// removed. See the Terminology section of
// http://www.webdav.org/specs/rfc4918.html#rfc.section.3
//
// There is a whitelist of the names of live properties. This package handles
// all live properties, and will only pass non-whitelisted names to the Patch
// method of DeadPropsHolder implementations.
type DeadPropsHolder interface {
	// DeadProps returns a copy of the dead properties held.
	DeadProps() map[xml.Name]Property

	// Patch patches the dead properties held.
	//
	// Patching is atomic; either all or no patches succeed. It returns (nil,
	// non-nil) if an internal server error occurred, otherwise the Propstats
	// collectively contain one Property for each proposed patch Property. If
	// all patches succeed, Patch returns a slice of length one and a Propstat
	// element with a 200 OK HTTP status code. If none succeed, for reasons
	// other than an internal server error, no Propstat has status 200 OK.
	//
	// For more details on when various HTTP status codes apply, see
	// http://www.webdav.org/specs/rfc4918.html#PROPPATCH-status
	Patch([]Proppatch) ([]Propstat, error)
}

// memPS implements an in-memory PropSystem. It supports all of the mandatory
// live properties of RFC 4918.
type memPS struct {
	fs FileSystem
	ls LockSystem
}

// NewMemPS returns a new in-memory PropSystem implementation.
func NewMemPS(fs FileSystem, ls LockSystem) PropSystem {
	return &memPS{
		fs: fs,
		ls: ls,
	}
}

// liveProps contains all supported, protected DAV: properties.
var liveProps = map[xml.Name]struct {
	// findFn implements the propfind function of this property. If nil,
	// it indicates a hidden property.
	findFn func(*memPS, string, os.FileInfo) (string, error)
	// dir is true if the property applies to directories.
	dir bool
}{
	xml.Name{Space: "DAV:", Local: "resourcetype"}: {
		findFn: (*memPS).findResourceType,
		dir:    true,
	},
	xml.Name{Space: "DAV:", Local: "displayname"}: {
		findFn: (*memPS).findDisplayName,
		dir:    true,
	},
	xml.Name{Space: "DAV:", Local: "getcontentlength"}: {
		findFn: (*memPS).findContentLength,
		dir:    true,
	},
	xml.Name{Space: "DAV:", Local: "getlastmodified"}: {
		findFn: (*memPS).findLastModified,
		dir:    true,
	},
	xml.Name{Space: "DAV:", Local: "creationdate"}: {
		findFn: nil,
		dir:    true,
	},
	xml.Name{Space: "DAV:", Local: "getcontentlanguage"}: {
		findFn: nil,
		dir:    true,
	},
	xml.Name{Space: "DAV:", Local: "getcontenttype"}: {
		findFn: (*memPS).findContentType,
		dir:    true,
	},
	xml.Name{Space: "DAV:", Local: "getetag"}: {
		findFn: (*memPS).findETag,
		// memPS implements ETag as the concatenated hex values of a file's
		// modification time and size. This is not a reliable synchronization
		// mechanism for directories, so we do not advertise getetag for
		// DAV collections.
		dir: false,
	},

	// TODO(nigeltao) Lock properties will be defined later.
	xml.Name{Space: "DAV:", Local: "lockdiscovery"}: {},
	xml.Name{Space: "DAV:", Local: "supportedlock"}: {},
}

func (ps *memPS) Find(name string, propnames []xml.Name) ([]Propstat, error) {
	f, err := ps.fs.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	isDir := fi.IsDir()

	var deadProps map[xml.Name]Property
	if dph, ok := f.(DeadPropsHolder); ok {
		deadProps = dph.DeadProps()
	}

	pstatOK := Propstat{Status: http.StatusOK}
	pstatNotFound := Propstat{Status: http.StatusNotFound}
	for _, pn := range propnames {
		// If this file has dead properties, check if they contain pn.
		if dp, ok := deadProps[pn]; ok {
			pstatOK.Props = append(pstatOK.Props, dp)
			continue
		}
		// Otherwise, it must either be a live property or we don't know it.
		if prop := liveProps[pn]; prop.findFn != nil && (prop.dir || !isDir) {
			innerXML, err := prop.findFn(ps, name, fi)
			if err != nil {
				return nil, err
			}
			pstatOK.Props = append(pstatOK.Props, Property{
				XMLName:  pn,
				InnerXML: []byte(innerXML),
			})
		} else {
			pstatNotFound.Props = append(pstatNotFound.Props, Property{
				XMLName: pn,
			})
		}
	}
	return makePropstats(pstatOK, pstatNotFound), nil
}

func (ps *memPS) Propnames(name string) ([]xml.Name, error) {
	f, err := ps.fs.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	isDir := fi.IsDir()

	var deadProps map[xml.Name]Property
	if dph, ok := f.(DeadPropsHolder); ok {
		deadProps = dph.DeadProps()
	}

	propnames := make([]xml.Name, 0, len(liveProps)+len(deadProps))
	for pn, prop := range liveProps {
		if prop.findFn != nil && (prop.dir || !isDir) {
			propnames = append(propnames, pn)
		}
	}
	for pn := range deadProps {
		propnames = append(propnames, pn)
	}
	return propnames, nil
}

func (ps *memPS) Allprop(name string, include []xml.Name) ([]Propstat, error) {
	propnames, err := ps.Propnames(name)
	if err != nil {
		return nil, err
	}
	// Add names from include if they are not already covered in propnames.
	nameset := make(map[xml.Name]bool)
	for _, pn := range propnames {
		nameset[pn] = true
	}
	for _, pn := range include {
		if !nameset[pn] {
			propnames = append(propnames, pn)
		}
	}
	return ps.Find(name, propnames)
}

func (ps *memPS) Patch(name string, patches []Proppatch) ([]Propstat, error) {
	conflict := false
loop:
	for _, patch := range patches {
		for _, p := range patch.Props {
			if _, ok := liveProps[p.XMLName]; ok {
				conflict = true
				break loop
			}
		}
	}
	if conflict {
		pstatForbidden := Propstat{
			Status:   http.StatusForbidden,
			XMLError: `<error xmlns="DAV:"><cannot-modify-protected-property/></error>`,
		}
		pstatFailedDep := Propstat{
			Status: StatusFailedDependency,
		}
		for _, patch := range patches {
			for _, p := range patch.Props {
				if _, ok := liveProps[p.XMLName]; ok {
					pstatForbidden.Props = append(pstatForbidden.Props, Property{XMLName: p.XMLName})
				} else {
					pstatFailedDep.Props = append(pstatFailedDep.Props, Property{XMLName: p.XMLName})
				}
			}
		}
		return makePropstats(pstatForbidden, pstatFailedDep), nil
	}

	f, err := ps.fs.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if dph, ok := f.(DeadPropsHolder); ok {
		ret, err := dph.Patch(patches)
		if err != nil {
			return nil, err
		}
		// http://www.webdav.org/specs/rfc4918.html#ELEMENT_propstat says that
		// "The contents of the prop XML element must only list the names of
		// properties to which the result in the status element applies."
		for _, pstat := range ret {
			for i, p := range pstat.Props {
				pstat.Props[i] = Property{XMLName: p.XMLName}
			}
		}
		return ret, nil
	}
	// The file doesn't implement the optional DeadPropsHolder interface, so
	// all patches are forbidden.
	pstat := Propstat{Status: http.StatusForbidden}
	for _, patch := range patches {
		for _, p := range patch.Props {
			pstat.Props = append(pstat.Props, Property{XMLName: p.XMLName})
		}
	}
	return []Propstat{pstat}, nil
}

func (ps *memPS) findResourceType(name string, fi os.FileInfo) (string, error) {
	if fi.IsDir() {
		return `<collection xmlns="DAV:"/>`, nil
	}
	return "", nil
}

func (ps *memPS) findDisplayName(name string, fi os.FileInfo) (string, error) {
	if slashClean(name) == "/" {
		// Hide the real name of a possibly prefixed root directory.
		return "", nil
	}
	return fi.Name(), nil
}

func (ps *memPS) findContentLength(name string, fi os.FileInfo) (string, error) {
	return strconv.FormatInt(fi.Size(), 10), nil
}

func (ps *memPS) findLastModified(name string, fi os.FileInfo) (string, error) {
	return fi.ModTime().Format(http.TimeFormat), nil
}

func (ps *memPS) findContentType(name string, fi os.FileInfo) (string, error) {
	f, err := ps.fs.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// This implementation is based on serveContent's code in the standard net/http package.
	ctype := mime.TypeByExtension(filepath.Ext(name))
	if ctype == "" {
		// Read a chunk to decide between utf-8 text and binary.
		var buf [512]byte
		n, _ := io.ReadFull(f, buf[:])
		ctype = http.DetectContentType(buf[:n])
		// Rewind file.
		_, err = f.Seek(0, os.SEEK_SET)
	}
	return ctype, err
}

func (ps *memPS) findETag(name string, fi os.FileInfo) (string, error) {
	return detectETag(fi), nil
}

// detectETag determines the ETag for the file described by fi.
func detectETag(fi os.FileInfo) string {
	// The Apache http 2.4 web server by default concatenates the
	// modification time and size of a file. We replicate the heuristic
	// with nanosecond granularity.
	return fmt.Sprintf(`"%x%x"`, fi.ModTime().UnixNano(), fi.Size())
}
