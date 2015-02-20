/*
 * Copyright 2015 Google Inc. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package filetree defines the filetree Service interface and a simple
// in-memory implementation.
package filetree

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"kythe/go/services/graphstore"
	"kythe/go/services/web"
	"kythe/go/util/kytheuri"
	"kythe/go/util/schema"

	srvpb "kythe/proto/serving_proto"
	spb "kythe/proto/storage_proto"
)

// Service provides an interface to explore a tree of VName files.
type Service interface {
	// Dir returns the FileDirectory for the given corpus/root/path.  nil is
	// returned for both the error and FileDirectory, if a directory is not found.
	Dir(corpus, root, path string) (*srvpb.FileDirectory, error)

	// CorporaRoots returns a map from corpus to known roots.
	CorporaRoots() (*srvpb.CorpusRoots, error)
}

// Map is a FileTree backed by an in-memory map.
type Map struct {
	// corpus -> root -> dirPath -> FileDirectory
	M map[string]map[string]map[string]*srvpb.FileDirectory
}

// NewMap returns an empty filetree map.
func NewMap() *Map {
	return &Map{make(map[string]map[string]map[string]*srvpb.FileDirectory)}
}

// Populate adds each file node in gs to m.
func (m *Map) Populate(gs graphstore.Service) error {
	start := time.Now()
	log.Println("Populating in-memory file tree")
	var total int
	if err := gs.Scan(&spb.ScanRequest{FactPrefix: schema.NodeKindFact},
		func(entry *spb.Entry) error {
			if entry.FactName == schema.NodeKindFact && string(entry.FactValue) == schema.FileKind {
				m.AddFile(entry.Source)
				total++
			}
			return nil
		}); err != nil {
		return fmt.Errorf("failed to Scan GraphStore for directory structure: %v", err)
	}
	log.Printf("Indexed %d files in %s", total, time.Since(start))
	return nil
}

// AddFile adds the given file VName to m.
func (m *Map) AddFile(file *spb.VName) {
	ticket := kytheuri.ToString(file)
	path := filepath.Join("/", file.Path)
	dir := m.ensureDir(file.Corpus, file.Root, filepath.Dir(path))
	dir.FileTicket = addToSet(dir.FileTicket, ticket)
}

// CorporaRoots implements part of the filetree.Service interface.
func (m *Map) CorporaRoots() (*srvpb.CorpusRoots, error) {
	cr := &srvpb.CorpusRoots{}
	for corpus, rootDirs := range m.M {
		var roots []string
		for root := range rootDirs {
			roots = append(roots, root)
		}
		cr.Corpus = append(cr.Corpus, &srvpb.CorpusRoots_Corpus{
			Corpus: corpus,
			Root:   roots,
		})
	}
	return cr, nil
}

// Dir implements part of the filetree.Service interface.
func (m *Map) Dir(corpus, root, path string) (*srvpb.FileDirectory, error) {
	roots := m.M[corpus]
	if roots == nil {
		return nil, nil
	}
	dirs := roots[root]
	if dirs == nil {
		return nil, nil
	}
	return dirs[path], nil
}

func (m *Map) ensureCorpusRoot(corpus, root string) map[string]*srvpb.FileDirectory {
	roots := m.M[corpus]
	if roots == nil {
		roots = make(map[string]map[string]*srvpb.FileDirectory)
		m.M[corpus] = roots
	}

	dirs := roots[root]
	if dirs == nil {
		dirs = make(map[string]*srvpb.FileDirectory)
		roots[root] = dirs
	}
	return dirs
}

func (m *Map) ensureDir(corpus, root, path string) *srvpb.FileDirectory {
	dirs := m.ensureCorpusRoot(corpus, root)
	dir := dirs[path]
	if dir == nil {
		dir = &srvpb.FileDirectory{}
		dirs[path] = dir

		if path != "/" {
			parent := m.ensureDir(corpus, root, filepath.Dir(path))
			uri := kytheuri.URI{
				Corpus: corpus,
				Root:   root,
				Path:   path,
			}
			parent.Subdirectory = addToSet(parent.Subdirectory, uri.String())
		}
	}
	return dir
}

func addToSet(strs []string, str string) []string {
	for _, s := range strs {
		if s == str {
			return strs
		}
	}
	return append(strs, str)
}

type corpusPath struct {
	Corpus string `json:"corpus"`
	Root   string `json:"root"`
	Path   string `json:"path"`
}

type webClient struct{ addr string }

// CorporaRoots implements part of the Service interface.
func (w *webClient) CorporaRoots() (*srvpb.CorpusRoots, error) {
	var reply srvpb.CorpusRoots
	return &reply, web.Call(w.addr, "corpusRoots", struct{}{}, &reply)
}

// Dir implements part of the Service interface.
func (w *webClient) Dir(corpus, root, path string) (*srvpb.FileDirectory, error) {
	var reply srvpb.FileDirectory
	return &reply, web.Call(w.addr, "dir", &corpusPath{
		Corpus: corpus,
		Root:   root,
		Path:   path,
	}, &reply)
}

// WebClient returns an filetree Service based on a remote web server.
func WebClient(addr string) Service { return &webClient{addr} }

// RegisterHTTPHandlers registers JSON HTTP handlers with mux using the given
// filetree Service.  The following methods with be exposed:
//
//   GET /corpusRoots
//     Response: JSON encoded serving.CorpusRoots
//   GET /dir
//     Request: JSON encoded {"corpus": <string>, "root": <string>, "path": <string>}
//     Response: JSON encoded serving.FileDirectory
//
// Note: /corpusRoots and /dir will return their responses as serialized
// protobufs if the "proto" query parameter is set.
func RegisterHTTPHandlers(ft Service, mux *http.ServeMux) {
	mux.HandleFunc("/corpusRoots", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("filetree.CorporaRoots:\t%s", time.Since(start))
		}()

		cr, err := ft.CorporaRoots()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := web.WriteResponse(w, r, cr); err != nil {
			log.Println(err)
		}
	})
	mux.HandleFunc("/dir", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("filetree.Dir:\t%s", time.Since(start))
		}()

		var req corpusPath
		if err := web.ReadJSONBody(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		dir, err := ft.Dir(req.Corpus, req.Root, req.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := web.WriteResponse(w, r, dir); err != nil {
			log.Println(err)
		}
	})
}
