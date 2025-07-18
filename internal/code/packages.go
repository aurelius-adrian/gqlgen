package code

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

var mode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedTypes |
	packages.NeedSyntax |
	packages.NeedTypesInfo |
	packages.NeedModule

type (
	// Packages is a wrapper around x/tools/go/packages that maintains a (hopefully prewarmed) cache of packages
	// that can be invalidated as writes are made and packages are known to change.
	Packages struct {
		packages              map[string]*packages.Package
		importToName          map[string]string
		loadErrors            []error
		buildFlags            []string
		packagesToCachePrefix string

		numLoadCalls int // stupid test steam. ignore.
		numNameCalls int // stupid test steam. ignore.
	}
	// Option is a function that can be passed to NewPackages to configure the package loader
	Option func(p *Packages)
)

// WithBuildTags option for NewPackages adds build tags to the packages.Load call
func WithBuildTags(tags ...string) func(p *Packages) {
	return func(p *Packages) {
		p.buildFlags = append(p.buildFlags, "-tags", strings.Join(tags, ","))
	}
}

func WithPreloadNames(importPaths ...string) func(p *Packages) {
	return func(p *Packages) {
		p.LoadAllNames(importPaths...)
	}
}

// PackagePrefixToCache option for NewPackages
// will not reset gqlgen packages in packages.Load call
func PackagePrefixToCache(prefixPath string) func(p *Packages) {
	return func(p *Packages) {
		p.packagesToCachePrefix = prefixPath
	}
}

// NewPackages creates a new packages cache
// It will load all packages in the current module, and any packages that are passed to Load or LoadAll
func NewPackages(opts ...Option) *Packages {
	p := &Packages{}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func dedupPackages(packages []string) []string {
	packageMap := make(map[string]struct{})
	dedupedPackages := make([]string, 0, len(packageMap))
	for _, p := range packages {
		if _, ok := packageMap[p]; ok {
			continue
		}
		packageMap[p] = struct{}{}
		dedupedPackages = append(dedupedPackages, p)
	}

	return dedupedPackages
}

func (p *Packages) CleanupUserPackages() {
	if p.packagesToCachePrefix == "" {
		// Cleanup all packages if we don't know which ones to keep
		p.packages = nil
	} else {
		// Don't clean up github.com/99designs/gqlgen prefixed packages,
		// they haven't changed and do not need to be reloaded
		// if you are using a fork, then you need to have customized
		// the prefix using PackagePrefixToCache
		var toRemove []string
		for k := range p.packages {
			if !strings.HasPrefix(k, p.packagesToCachePrefix) {
				toRemove = append(toRemove, k)
			}
		}
		for _, k := range toRemove {
			delete(p.packages, k)
		}
	}
}

// ReloadAll will call LoadAll after clearing the package cache, so we can reload
// packages in the case that the packages have changed
func (p *Packages) ReloadAll(importPaths ...string) []*packages.Package {
	if p.packages != nil {
		p.CleanupUserPackages()
	}
	return p.LoadAll(importPaths...)
}

// LoadAll will call packages.Load and return the package data for the given packages,
// but if the package already have been loaded it will return cached values instead.
func (p *Packages) LoadAll(importPaths ...string) []*packages.Package {
	if p.packages == nil {
		p.packages = map[string]*packages.Package{}
	}

	missing := make([]string, 0, len(importPaths))
	for _, path := range importPaths {
		if _, ok := p.packages[path]; ok {
			continue
		}
		missing = append(missing, path)
	}

	if len(missing) > 0 {
		p.numLoadCalls++
		pkgs, err := packages.Load(&packages.Config{
			Mode:       mode,
			BuildFlags: p.buildFlags,
		}, missing...)
		if err != nil {
			p.loadErrors = append(p.loadErrors, err)
		}

		for _, pkg := range pkgs {
			p.addToCache(pkg)
		}
	}

	res := make([]*packages.Package, 0, len(importPaths))
	for _, path := range importPaths {
		res = append(res, p.packages[NormalizeVendor(path)])
	}
	return res
}

func (p *Packages) addToCache(pkg *packages.Package) {
	imp := NormalizeVendor(pkg.PkgPath)
	p.packages[imp] = pkg
}

// Load works the same as LoadAll, except a single package at a time.
func (p *Packages) Load(importPath string) *packages.Package {
	// Quick cache check first to avoid expensive allocations of LoadAll()
	if p.packages != nil {
		if pkg, ok := p.packages[importPath]; ok {
			return pkg
		}
	}

	pkgs := p.LoadAll(importPath)
	if len(pkgs) == 0 {
		return nil
	}
	return pkgs[0]
}

// LoadWithTypes tries a standard load, which may not have enough type info (TypesInfo== nil) available if the imported package is a
// second order dependency. Fortunately this doesnt happen very often, so we can just issue a load when we detect it.
func (p *Packages) LoadWithTypes(importPath string) *packages.Package {
	pkg := p.Load(importPath)
	if pkg == nil || pkg.TypesInfo == nil {
		p.numLoadCalls++
		pkgs, err := packages.Load(&packages.Config{
			Mode:       mode,
			BuildFlags: p.buildFlags,
		}, importPath)
		if err != nil {
			p.loadErrors = append(p.loadErrors, err)
			return nil
		}
		p.addToCache(pkgs[0])
		pkg = pkgs[0]
	}
	return pkg
}

// LoadAllNames will call packages.Load with the NeedName mode only and will store the package name in a cache.
// it does not return any package data, but after calling this you can call NameForPackage to get the package name without loading the full package data.
func (p *Packages) LoadAllNames(importPaths ...string) {
	importPaths = dedupPackages(importPaths)
	missing := make([]string, 0, len(importPaths))
	for _, importPath := range importPaths {
		if importPath == "" {
			panic(errors.New("import path can not be empty"))
		}

		if p.importToName == nil {
			p.importToName = map[string]string{}
		}

		importPath = NormalizeVendor(importPath)

		// if it's in the name cache use it
		if name := p.importToName[importPath]; name != "" {
			continue
		}

		// otherwise we might have already loaded the full package data for it cached
		pkg := p.packages[importPath]
		if pkg != nil {
			if _, ok := p.importToName[importPath]; !ok {
				p.importToName[importPath] = pkg.Name
			}

			continue
		}

		missing = append(missing, importPath)
	}

	if len(missing) > 0 {
		pkgs, err := packages.Load(&packages.Config{
			Mode:       packages.NeedName,
			BuildFlags: p.buildFlags,
		}, missing...)

		if err != nil {
			p.loadErrors = append(p.loadErrors, err)
		}

		for _, pkg := range pkgs {
			if pkg.Name == "" {
				pkg.Name = SanitizePackageName(filepath.Base(pkg.PkgPath))
			}

			p.importToName[pkg.PkgPath] = pkg.Name
		}
	}
}

// NameForPackage looks up the package name from the package stanza in the go files at the given import path.
func (p *Packages) NameForPackage(importPath string) string {
	p.numNameCalls++
	p.LoadAllNames(importPath)

	importPath = NormalizeVendor(importPath)
	return p.importToName[importPath]
}

// Evict removes a given package import path from the cache. Further calls to Load will fetch it from disk.
func (p *Packages) Evict(importPath string) {
	delete(p.packages, importPath)
}

func (p *Packages) ModTidy() error {
	p.packages = nil
	tidyCmd := exec.Command("go", "mod", "tidy")
	tidyCmd.Stdout = os.Stdout
	tidyCmd.Stderr = os.Stdout
	if err := tidyCmd.Run(); err != nil {
		return fmt.Errorf("go mod tidy failed: %w", err)
	}
	return nil
}

// Errors returns any errors that were returned by Load, either from the call itself or any of the loaded packages.
func (p *Packages) Errors() PkgErrors {
	res := append([]error{}, p.loadErrors...)
	for _, pkg := range p.packages {
		for _, err := range pkg.Errors {
			res = append(res, err)
		}
	}
	return res
}

func (p *Packages) Count() int {
	return len(p.packages)
}

type PkgErrors []error

func (p PkgErrors) Error() string {
	var b bytes.Buffer
	b.WriteString("packages.Load: ")
	for _, e := range p {
		b.WriteString(e.Error() + "\n")
	}
	return b.String()
}
