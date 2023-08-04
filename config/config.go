//  Copyright (c) 2023 Uber Technologies, Inc.
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

// Package config implements the configurations for NilAway.
package config

import (
	"flag"
	"go/ast"
	"go/types"
	"reflect"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Config is the struct that stores the user-configurable options for NilAway.
type Config struct {
	// PrettyPrint indicates whether the error messages should be pretty printed.
	PrettyPrint bool
	// includePkgs is the list of packages to analyze.
	includePkgs []string
	// excludePkgs is the list of packages to exclude from analysis. Exclude list takes
	// precedence over the include list.
	excludePkgs []string
	// excludeFileDocStrings is the list of doc strings that, if they appear in the file doc
	// string, will cause the file to be excluded from analysis. Examples include "@generated" and
	// "Code generated by".
	excludeFileDocStrings []string
}

// IsPkgInScope returns true iff the passed package is in scope for analysis, i.e., it is in the
// configured include list but not in the exclude list.
func (c *Config) IsPkgInScope(pkg *types.Package) bool {
	if pkg == nil {
		return false
	}

	for _, include := range c.includePkgs {
		if !strings.HasPrefix(pkg.Path(), include) {
			continue
		}

		for _, exclude := range c.excludePkgs {
			if strings.HasPrefix(pkg.Path(), exclude) {
				return false
			}
		}
		return true
	}

	return false
}

// IsFileInScope returns true iff we should analyze the file. It checks the docstring of the file
// and returns false if any of the strings in ExcludeFileDocStrings appear in the file docstring.
func (c *Config) IsFileInScope(file *ast.File) bool {
	// Fast return if there is no exclude list.
	if len(c.excludeFileDocStrings) == 0 {
		return true
	}

	for _, comment := range file.Comments {
		// The comment group here contains all comments in the file. However, we should only check
		// the comments before the package name (e.g., `package Foo`) line.
		if comment.Pos() > file.Name.Pos() {
			continue
		}

		for _, exclude := range c.excludeFileDocStrings {
			if strings.Contains(comment.Text(), exclude) {
				return false
			}
		}
	}
	return true
}

const _doc = `nilaway_config analyzer is responsible to take configurations (flags) for NilAway execution.
It does not run any analysis and is only meant to be used as a dependency for the sub-analyzers of 
NilAway to share the same configurations. 
`

// Analyzer is the pseudo-analyzer that takes the flags and share them among the sub-analyzers of
// NilAway. All sub-analyzers have to depend on this analyzer to get the flags.
//
// This is required due to our multi-sub-analyzer architecture in NilAway: by the time the
// top-level analyzer is run, the analysis is already done (by the sub-analyzers), hence the flags
// controlling the analysis behaviors will be meaningless. Instead, we add this pseudo-analyzer to
// run first (since all sub-analyzers will depend on it), and make the flags available via its
// return value.
//
// Unfortunately, this also means for some analyzer drivers (such as nogo), flags will have to be
// specified for this pseudo-analyzer ("nilaway_config"), and the error suppression lists will have
// to be specified for the top-level analyzer ("nilaway") since that is the one that outputs errors.
var Analyzer = &analysis.Analyzer{
	Name:       "nilaway_config",
	Doc:        _doc,
	Run:        run,
	Flags:      newFlagSet(),
	ResultType: reflect.TypeOf((*Config)(nil)),
}

const (
	// PrettyPrintFlag is the flag for pretty printing the error messages.
	PrettyPrintFlag = "pretty-print"
	// IncludePkgsFlag is the flag name for include package prefixes.
	IncludePkgsFlag = "include-pkgs"
	// ExcludePkgsFlag is the flag name for exclude package prefixes.
	ExcludePkgsFlag = "exclude-pkgs"
	// ExcludeFileDocStringsFlag is the flag name for the docstrings that exclude files from analysis.
	ExcludeFileDocStringsFlag = "exclude-file-docstrings"
)

// newFlagSet returns a flag set to be used in the nilaway config analyzer.
func newFlagSet() flag.FlagSet {
	fs := flag.NewFlagSet("nilaway_config", flag.ExitOnError)

	// We do not keep the returned pointer to the flags because we will not use them directly here.
	// Instead, we will use the flags through the analyzer's Flags field later.
	_ = fs.Bool(PrettyPrintFlag, true, "Pretty print the error messages")
	_ = fs.String(IncludePkgsFlag, "", "Comma-separated list of packages to analyze")
	_ = fs.String(ExcludePkgsFlag, "", "Comma-separated list of packages to exclude from analysis")
	_ = fs.String(ExcludeFileDocStringsFlag, "", "Comma-separated list of docstrings to exclude from analysis")

	return *fs
}

func run(pass *analysis.Pass) (any, error) {
	// Set up default values for the config.
	conf := &Config{
		PrettyPrint: true,
		// If the user does not provide an include list, we give an empty package prefix to catch
		// all packages.
		includePkgs: []string{""},
	}

	// Override default values if the user provides flags.
	if prettyPrint, ok := pass.Analyzer.Flags.Lookup(PrettyPrintFlag).Value.(flag.Getter).Get().(bool); ok {
		conf.PrettyPrint = prettyPrint
	}
	if include, ok := pass.Analyzer.Flags.Lookup(IncludePkgsFlag).Value.(flag.Getter).Get().(string); ok && include != "" {
		conf.includePkgs = strings.Split(include, ",")
	}
	if exclude, ok := pass.Analyzer.Flags.Lookup(ExcludePkgsFlag).Value.(flag.Getter).Get().(string); ok && exclude != "" {
		conf.excludePkgs = strings.Split(exclude, ",")
	}
	if docstrings, ok := pass.Analyzer.Flags.Lookup(ExcludeFileDocStringsFlag).Value.(flag.Getter).Get().(string); ok && docstrings != "" {
		conf.excludeFileDocStrings = strings.Split(docstrings, ",")
	}

	return conf, nil
}
