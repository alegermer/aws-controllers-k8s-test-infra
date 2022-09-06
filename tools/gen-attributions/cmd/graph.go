// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

var (
	ErrorGoModFileNotFound = errors.New("module file (go.mod) not found")
	ErrorLicenseNotFound   = errors.New("LICENSE file not found")
)

// graphBuilder helps building the dependency graph of a given go module.
type graphBuilder struct {
	logger *logrus.Logger
	// modulesCache is used to avoid downloading/parsing already seen modules.
	modulesCache map[string]*Module
	// classifier for licenses
	lc *licenseClassifierWrapper
}

// newGraphBuilder instantiate a new graphBuilder
func newGraphBuilder(logger *logrus.Logger, licenseClassificationTreshold float64) (*graphBuilder, error) {
	lc, err := newLicenseClassifier(licenseClassificationTreshold)
	if err != nil {
		return nil, err
	}
	return &graphBuilder{
		modulesCache: make(map[string]*Module),
		logger:       logger,
		lc:           lc,
	}, nil
}

func loadModulesFromModFile(modRoot *ModRoot) []*Module {
	mod := modRoot.ModFile
	var requiredModules []*Module

	replacementMap := make(map[module.Version]*module.Version)
	for _, r := range mod.Replace {
		replacementMap[r.Old] = &r.New
	}

	for _, r := range mod.Require {

		reqVersion := &r.Mod
		replacedBy := replacementMap[*reqVersion]

		requiredModules = append(requiredModules, &Module{
			Version:    reqVersion,
			ReplacedBy: replacedBy,
		})
	}
	return requiredModules
}

// buildGraph takes a modfile a max depth and proceeds into building the
// dependency Tree. maxDepth is the depth at which the graphBuilder will
// will stop exploring the dependency graph.
func (gb *graphBuilder) buildGraph(modRoot *ModRoot, maxDepth int) (*Tree, error) {

	gb.logger.Debug("Started building the dependency graph")

	requiredModules := loadModulesFromModFile(modRoot)

	err := gb.buildModulesDependencyGraph(modRoot, requiredModules, 0, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("cannot build modules tree: %v", err)
	}

	gb.logger.Debug("Dependency graph built successfully")

	return &Tree{
		Root: &Module{
			Version:      &modRoot.ModFile.Module.Mod,
			Dependencies: requiredModules,
		},
	}, nil
}

func (gb *graphBuilder) getModuleFromCache(mod *Module) (*Module, bool) {
	m, found := gb.modulesCache[mod.moduleID()]
	return m, found
}

func (gb *graphBuilder) cacheModule(m *Module) {
	gb.modulesCache[m.moduleID()] = m
}

// buildModulesDependencyGraph takes a list module IDs (version and path) and
// returns a list *Module objects, containing the corresponding licenses and
// dependencies.
func (gb *graphBuilder) buildModulesDependencyGraph(
	modRoot *ModRoot,
	mods []*Module,
	depth int,
	maxDepth int,
) error {
	for _, mod := range mods {
		if depth == maxDepth {
			continue
		}

		gb.logger.Debugf("Exploring module %+v", mod)

		// first check the cache
		cachedMod, cached := gb.getModuleFromCache(mod)
		if cached {
			mod.Dependencies = cachedMod.Dependencies
			mod.License = cachedMod.License
			continue
		}

		// else, recursively build the Module object and cache it
		childModRoot, license, childModules, err := gb.extractLicenseAndRequiredModules(modRoot, mod)
		if err != nil {
			return err
		}
		gb.logger.Debugf("Found %s license and %d required modules", mod, len(childModules))

		var licenseType string
		if lenient && len(license) == 0 {
			gb.logger.Warningf("No license found for %v", mod.Version)
			licenseType = "Unknown"
			license = []byte(fmt.Sprintf("WARNING: YOU MUST OVERRIDE THIS WITH A PROPER LICENSE\nUNKNOWN LICENSE FOR %q", mod.Version))
		} else {
			licenseType, err = gb.lc.detectLicense(license)
			if err != nil {
				return err
			}
		}

		mod.License = &License{
			Data: license,
			Name: licenseType,
		}

		// TODO: (asgermer) remove quick & dirty short-circuit here
		if childModRoot != nil {
			err = gb.buildModulesDependencyGraph(
				childModRoot, childModules, depth+1, maxDepth,
			)
			if err != nil {
				return err
			}
			mod.Dependencies = childModules
		}

		// cache the module
		gb.cacheModule(mod)
		gb.logger.Debugf("Cached %s module", mod)
	}

	return nil
}

// extractLicenseAndRequiredModules downloads a module from the configured
// go proxy and extract the license and the required modules from it go.mod
// file.
func (gb *graphBuilder) extractLicenseAndRequiredModules(
	modRoot *ModRoot,
	mod *Module,
) (*ModRoot, []byte, []*Module, error) {

	// TODO: (asgermer) this is currently assuming all 'replace' statements point to local paths but this is not true
	if mod.ReplacedBy != nil {
		fullPath := filepath.Join(modRoot.RootPath, mod.ReplacedBy.Path)
		gb.logger.Debugf("Looking at %v", fullPath)

		files, err := ioutil.ReadDir(fullPath)
		if err != nil {
			log.Fatal(err)
		}

		var licenseFile []byte
		var childGoModFilename string
		for _, file := range files {
			if !file.IsDir() && isLicenseFilename("/"+file.Name()) {
				licenseFile, err = ioutil.ReadFile(filepath.Join(fullPath, file.Name()))
			} else if file.Name() == "go.mod" {
				childGoModFilename = filepath.Join(fullPath, file.Name())
			}
		}

		childModRoot, childModules, err := getRequiredModulesFromFile(childGoModFilename)

		return childModRoot, licenseFile, childModules, nil
	}

	// Download the module
	gb.logger.Debugf("Downloading %v content", mod)
	moduleZip, err := downloadModule(mod.Version)
	if err != nil && lenient {
		gb.logger.Warningf("Got download error: %v", err)
		return nil, []byte{}, []*Module{}, nil
	} else if err != nil {
		return nil, nil, nil, err
	}

	// extract the license bytes
	gb.logger.Debugf("Extracting %v license", mod)
	license, err := extractLicense(mod.Version.String(), moduleZip)
	if err != nil && !(err == ErrorLicenseNotFound && lenient) {
		return nil, nil, nil, err
	}

	// extract the required modules from go.mod file
	gb.logger.Debugf("Extracting %v required modules", mod)
	modRoot, requiredModules, err := getRequiredModules(mod.Version.Version, moduleZip)
	if err != nil && err != ErrorGoModFileNotFound {
		return nil, nil, nil, err
	}

	return modRoot, license, requiredModules, nil
}

// extractLicense looks in a module zipFile and returns the content of its
// license
func extractLicense(moduleFullName string, zipfile []byte) ([]byte, error) {
	zipReader, err := zip.NewReader(bytes.NewReader(zipfile), int64(len(zipfile)))
	if err != nil {
		return nil, err
	}

	for _, file := range zipReader.File {
		cleanFileName := strings.TrimPrefix(file.Name, strings.ToLower(moduleFullName))
		if isLicenseFilename(cleanFileName) {
			f, err := file.Open()
			if err != nil {
				return nil, fmt.Errorf("cannot open license file: %v", err)
			}
			defer f.Close()
			b, err := ioutil.ReadAll(f)
			if err != nil {
				return nil, fmt.Errorf("cannot read license file content: %v", err)
			}
			return b, nil
		}
	}

	return nil, ErrorLicenseNotFound
}

// isLicenseFilename returns true if the filename most likely contains a license.
func isLicenseFilename(filename string) bool {
	name := strings.ToLower(filename)
	// NOTE(a-hilaly) are we missing any other cases?

	for _, l := range []string{
		"/license.txt",
		"/license",
		"/license.md",
		"/copying",
	} {
		if name == l {
			return true
		}
	}

	return false
}

// extractLicense looks in a module zipFile and returns the list of required modules.
func getRequiredModules(moduleFullName string, zipfile []byte) (*ModRoot, []*Module, error) {
	zipReader, err := zip.NewReader(bytes.NewReader(zipfile), int64(len(zipfile)))
	if err != nil {
		return nil, nil, err
	}
	for _, file := range zipReader.File {
		cleanFileName := strings.TrimPrefix(file.Name, moduleFullName)
		if cleanFileName == "/go.mod" {
			f, err := file.Open()
			if err != nil {
				return nil, nil, fmt.Errorf("cannot open mod file: %v", err)
			}
			defer f.Close()
			b, err := ioutil.ReadAll(f)
			if err != nil {
				return nil, nil, fmt.Errorf("cannot read file content: %v", err)
			}
			return getRequiredModulesFromBytes(b)
		}
	}

	return nil, nil, ErrorGoModFileNotFound
}

// extractLicense parses a go module file and returns the list of required modules.
func getRequiredModulesFromBytes(bytes []byte) (*ModRoot, []*Module, error) {
	goMod, err := modfile.Parse("", bytes, nil)
	if err != nil {
		return nil, nil, err
	}
	modRoot := &ModRoot{ModFile: goMod}
	modules := loadModulesFromModFile(modRoot)
	return modRoot, modules, nil
}

func getRequiredModulesFromFile(filename string) (*ModRoot, []*Module, error) {
	bytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, nil, err
	}
	goMod, err := modfile.Parse("", bytes, nil)
	if err != nil {
		return nil, nil, err
	}
	modRoot := &ModRoot{ModFile: goMod, RootPath: filepath.Dir(filename)}
	modules := loadModulesFromModFile(modRoot)
	return modRoot, modules, nil
}
