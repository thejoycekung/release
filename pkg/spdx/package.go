/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spdx

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"html/template"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"sigs.k8s.io/release-utils/hash"
	"sigs.k8s.io/release-utils/util"
)

var packageTemplate = `##### Package: {{ .Name }}

{{ if .Name }}PackageName: {{ .Name }}
{{ end -}}
{{ if .ID }}SPDXID: {{ .ID }}
{{ end -}}
{{- if .Checksum -}}
{{- range $key, $value := .Checksum -}}
{{ if . }}PackageChecksum: {{ $key }}: {{ $value }}
{{ end -}}
{{- end -}}
{{- end -}}
PackageDownloadLocation: {{ if .DownloadLocation }}{{ .DownloadLocation }}{{ else }}NONE{{ end }}
FilesAnalyzed: {{ .FilesAnalyzed }}
{{ if .VerificationCode }}PackageVerificationCode: {{ .VerificationCode }}
{{ end -}}
PackageLicenseConcluded: {{ if .LicenseConcluded }}{{ .LicenseConcluded }}{{ else }}NOASSERTION{{ end }}
{{ if .FileName }}PackageFileName: {{ .FileName }}
{{ end -}}
{{ if .LicenseInfoFromFiles }}{{- range $key, $value := .LicenseInfoFromFiles -}}PackageLicenseInfoFromFiles: {{ $value }}
{{ end -}}
{{ end -}}
{{ if .Version }}PackageVersion: {{ .Version }}
{{ end -}}
PackageLicenseDeclared: {{ if .LicenseDeclared }}{{ .LicenseDeclared }}{{ else }}NOASSERTION{{ end }}
PackageCopyrightText: {{ if .CopyrightText }}<text>{{ .CopyrightText }}
</text>{{ else }}NOASSERTION{{ end }}

`

// Package groups a set of files
type Package struct {
	sync.RWMutex
	FilesAnalyzed        bool     // true
	Name                 string   // hello-go-src
	ID                   string   // SPDXRef-Package-hello-go-src
	DownloadLocation     string   // git@github.com:swinslow/spdx-examples.git#example6/content/src
	VerificationCode     string   // 6486e016b01e9ec8a76998cefd0705144d869234
	LicenseConcluded     string   // LicenseID o NOASSERTION
	LicenseInfoFromFiles []string // GPL-3.0-or-later
	LicenseDeclared      string   // GPL-3.0-or-later
	LicenseComments      string   // record any relevant background information or analysis that went in to arriving at the Concluded License
	CopyrightText        string   // string NOASSERTION
	Version              string   // Package version
	FileName             string   // Name of the package
	SourceFile           string   // Source file for the package (taball for images, rpm, deb, etc)

	// Supplier: the actual distribution source for the package/directory
	Supplier struct {
		Person       string // person name and optional (<email>)
		Organization string // organization name and optional (<email>)
	}
	// Originator: For example, the SPDX file identifies the package glibc and Red Hat as the Package Supplier,
	// but the Free Software Foundation is the Package Originator.
	Originator struct {
		Person       string // person name and optional (<email>)
		Organization string // organization name and optional (<email>)
	}
	// Subpackages contained
	Packages     map[string]*Package // Sub packages conatined in this pkg
	Files        map[string]*File    // List of files
	Checksum     map[string]string   // Checksum of the package
	Dependencies map[string]*Package // Packages marked as dependencies

	options *PackageOptions // Options
}

func NewPackage() (p *Package) {
	p = &Package{
		options: &PackageOptions{},
	}
	return p
}

type PackageOptions struct {
	WorkDir string // Working directory to read files from
}

func (p *Package) Options() *PackageOptions {
	return p.options
}

// ReadSourceFile reads the source file for the package and populates
//  the package fields derived from it (Checksums and FileName)
func (p *Package) ReadSourceFile(path string) error {
	if !util.Exists(path) {
		return errors.New("unable to find package source file")
	}
	s256, err := hash.SHA256ForFile(path)
	if err != nil {
		return errors.Wrap(err, "getting source file sha256")
	}
	s512, err := hash.SHA512ForFile(path)
	if err != nil {
		return errors.Wrap(err, "getting source file sha512")
	}
	p.Checksum = map[string]string{
		"SHA256": s256,
		"SHA512": s512,
	}
	p.SourceFile = path
	p.FileName = strings.TrimPrefix(path, p.Options().WorkDir+string(filepath.Separator))
	return nil
}

// AddFile adds a file contained in the package
func (p *Package) AddFile(file *File) error {
	p.Lock()
	defer p.Unlock()
	if p.Files == nil {
		p.Files = map[string]*File{}
	}
	// If file does not have an ID, we try to build one
	// by hashing the file name
	if file.ID == "" {
		if file.Name == "" {
			return errors.New("unable to generate file ID, filename not set")
		}
		if p.Name == "" {
			return errors.New("unable to generate file ID, package not set")
		}
		h := sha1.New()
		if _, err := h.Write([]byte(p.Name + ":" + file.Name)); err != nil {
			return errors.Wrap(err, "getting sha1 of filename")
		}
		file.ID = "SPDXRef-File-" + fmt.Sprintf("%x", h.Sum(nil))
	}
	p.Files[file.ID] = file
	return nil
}

// preProcessSubPackage performs a basic check on a package
// to ensure it can be added as a subpackage, trying to infer
// missing data when possible
func (p *Package) preProcessSubPackage(pkg *Package) error {
	if pkg.ID == "" {
		// If we so not have an ID but have a name generate it fro there
		reg := regexp.MustCompile(validNameCharsRe)
		id := reg.ReplaceAllString(pkg.Name, "")
		if id != "" {
			pkg.ID = "SPDXRef-Package-" + id
		}
	}
	if pkg.ID == "" {
		return errors.New("package name is needed to add a new package")
	}
	if _, ok := p.Packages[pkg.ID]; ok {
		return errors.New("a package named " + pkg.ID + " already exists as a subpackage")
	}

	if _, ok := p.Dependencies[pkg.ID]; ok {
		return errors.New("a package named " + pkg.ID + " already exists as a dependency")
	}

	return nil
}

// AddPackage adds a new subpackage to a package
func (p *Package) AddPackage(pkg *Package) error {
	if p.Packages == nil {
		p.Packages = map[string]*Package{}
	}

	if err := p.preProcessSubPackage(pkg); err != nil {
		return errors.Wrap(err, "performing subpackage preprocessing")
	}

	p.Packages[pkg.ID] = pkg
	return nil
}

// AddDependency adds a new subpackage as a dependency
func (p *Package) AddDependency(pkg *Package) error {
	if p.Dependencies == nil {
		p.Dependencies = map[string]*Package{}
	}

	if err := p.preProcessSubPackage(pkg); err != nil {
		return errors.Wrap(err, "performing subpackage preprocessing")
	}

	p.Dependencies[pkg.ID] = pkg
	return nil
}

// Render renders the document fragment of the package
func (p *Package) Render() (docFragment string, err error) {
	var buf bytes.Buffer
	tmpl, err := template.New("package").Parse(packageTemplate)
	if err != nil {
		return "", errors.Wrap(err, "parsing package template")
	}

	// If files were analyzed, calculate the verification which
	// is a sha1sum from all sha1 checksumf from included friles.
	//
	// Since we are already doing it, we use the same loop to
	// collect license tags to express them in the LicenseInfoFromFiles
	// entry of the SPDX package:
	filesTagList := []string{}
	if p.FilesAnalyzed {
		if len(p.Files) == 0 {
			return docFragment, errors.New("unable to get package verification code, package has no files")
		}
		shaList := []string{}
		for _, f := range p.Files {
			if f.Checksum == nil {
				return docFragment, errors.New("unable to render package, file has no checksums")
			}
			if _, ok := f.Checksum["SHA1"]; !ok {
				return docFragment, errors.New("unable to render package, files were analyzed but some do not have sha1 checksum")
			}
			shaList = append(shaList, f.Checksum["SHA1"])

			// Collect the license tags
			if f.LicenseInfoInFile != "" {
				collected := false
				for _, tag := range filesTagList {
					if tag == f.LicenseInfoInFile {
						collected = true
						break
					}
				}
				if !collected {
					filesTagList = append(filesTagList, f.LicenseInfoInFile)
				}
			}
		}
		sort.Strings(shaList)
		h := sha1.New()
		if _, err := h.Write([]byte(strings.Join(shaList, ""))); err != nil {
			return docFragment, errors.Wrap(err, "getting sha1 verification of files")
		}
		p.VerificationCode = fmt.Sprintf("%x", h.Sum(nil))

		for _, tag := range filesTagList {
			if tag != NONE && tag != NOASSERTION {
				p.LicenseInfoFromFiles = append(p.LicenseInfoFromFiles, tag)
			}
		}

		// If no license tags where collected from files, then
		// the BOM has to express "NONE" in the LicenseInfoFromFiles
		// section to be compliant:
		if len(filesTagList) == 0 {
			p.LicenseInfoFromFiles = append(p.LicenseInfoFromFiles, NONE)
		}
	}

	// Run the template to verify the output.
	if err := tmpl.Execute(&buf, p); err != nil {
		return "", errors.Wrap(err, "executing spdx package template")
	}

	docFragment = buf.String()

	for _, f := range p.Files {
		fileFragment, err := f.Render()
		if err != nil {
			return "", errors.Wrap(err, "rendering file "+f.Name)
		}
		docFragment += fileFragment
		docFragment += fmt.Sprintf("Relationship: %s CONTAINS %s\n\n", p.ID, f.ID)
	}

	// Print the contained sub packages
	if p.Packages != nil {
		for _, pkg := range p.Packages {
			pkgDoc, err := pkg.Render()
			if err != nil {
				return "", errors.Wrap(err, "rendering pkg "+pkg.Name)
			}

			docFragment += pkgDoc
			docFragment += fmt.Sprintf("Relationship: %s CONTAINS %s\n\n", p.ID, pkg.ID)
		}
	}

	// Print the contained dependencies
	if p.Dependencies != nil {
		for _, pkg := range p.Dependencies {
			pkgDoc, err := pkg.Render()
			if err != nil {
				return "", errors.Wrap(err, "rendering pkg "+pkg.Name)
			}

			docFragment += pkgDoc
			docFragment += fmt.Sprintf("Relationship: %s DEPENDS_ON %s\n\n", p.ID, pkg.ID)
		}
	}
	return docFragment, nil
}
