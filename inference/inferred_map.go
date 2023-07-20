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

package inference

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"go/types"

	"go.uber.org/nilaway/annotation"
	"golang.org/x/tools/go/analysis"
)

// An InferredMap is the state accumulated by multi-package inference. It's
// field `Mapping` maps a set of known annotation sites to InferredAnnotationVals - which can
// be either a fixed bool value along with explanation for why it was fixed - an DeterminedVal
// - or an UndeterminedVal indicating that site's place in the known implication graph
// between underconstrained sites. The set of sites mapped to UndeterminedBoolVals is guaranteed
// to be closed under following `Implicant`s and `Implicate`s pointers.
//
// Additionally, a field upstreamMapping is stored indicating a stable copy of the information
// gleaned from upstream packages. Both mapping and upstreamMapping are initially populated
// with the same informations, but observation functions (observeSiteExplanation and observeImplication)
// add information only to Mapping. On export, iterations combined with calls to
// inferredValDiff on shared keys is used to ensure that only
// information present in `Mapping` but not `UpstreamMapping` is exported.
type InferredMap struct {
	upstreamMapping map[primitiveSite]InferredVal
	mapping         map[primitiveSite]InferredVal
}

// newInferredMap returns a new, empty InferredMap.
func newInferredMap() *InferredMap {
	return &InferredMap{
		upstreamMapping: make(map[primitiveSite]InferredVal),
		mapping:         make(map[primitiveSite]InferredVal),
	}
}

// AFact allows InferredAnnotationMaps to be imported and exported via the Facts mechanism.
func (*InferredMap) AFact() {}

// String returns a string representation of the InferredMap for debugging purposes.
func (i *InferredMap) String() string {

	valStr := func(val InferredVal) string {
		switch val := val.(type) {
		case *DeterminedVal:
			return fmt.Sprintf("%T", val.Bool)
		case *UndeterminedVal:
			implicants, implicates := "", ""
			for implicant := range val.Implicants {
				implicants += fmt.Sprintf("%s-> ", implicant.String())
			}
			for implicate := range val.Implicates {
				implicates += fmt.Sprintf("->%s ", implicate.String())
			}
			return fmt.Sprintf("[%s && %s]", implicants, implicates)
		}
		return ""
	}

	out := "{"
	for site, val := range i.mapping {
		out += fmt.Sprintf("%s: %s, ", site.String(), valStr(val))
	}
	return out + "}"
}

// Load returns the value stored in the map for an annotation site, or nil if no value is present.
// The ok result indicates whether value was found in the map.
func (i *InferredMap) Load(site primitiveSite) (value InferredVal, ok bool) {
	value, ok = i.mapping[site]
	return
}

// StoreDetermined sets the inferred value for an annotation site.
func (i *InferredMap) StoreDetermined(site primitiveSite, value ExplainedBool) {
	i.mapping[site] = &DeterminedVal{Bool: value}
	return
}

// StoreImplication stores an implication edge between the `from` and `to` annotation sites in the
// graph with the assertion for error reporting.
func (i *InferredMap) StoreImplication(from primitiveSite, to primitiveSite, assertion primitiveFullTrigger) {
	// First create UndeterminedVal in the map if it does not exist yet.
	for _, site := range [...]primitiveSite{from, to} {
		if _, ok := i.mapping[site]; !ok {
			i.mapping[site] = &UndeterminedVal{
				Implicates: newSitesWithAssertions(),
				Implicants: newSitesWithAssertions(),
			}
		}
	}

	i.mapping[from].(*UndeterminedVal).Implicates.addSiteWithAssertion(to, assertion)
	i.mapping[to].(*UndeterminedVal).Implicants.addSiteWithAssertion(from, assertion)
}

// Len returns the number of annotation sites currently stored in the map.
func (i *InferredMap) Len() int {
	return len(i.mapping)
}

// Range calls f sequentially for each annotation site and inferred value present in the map.
// If f returns false, range stops the iteration.
func (i *InferredMap) Range(f func(primitiveSite, InferredVal) bool) {
	for site, value := range i.mapping {
		if !f(site, value) {
			return
		}
	}
}

// Export only encodes new information not already present in the upstream maps, and it does not
// encode all (in the go sense; i.e. capitalized) annotation sites (See chooseSitesToExport).
// This ensures that only _incremental_ information is exported by this package and plays a _vital_
// role in minimizing build output.
func (i *InferredMap) Export(pass *analysis.Pass) {
	if len(i.mapping) == 0 {
		return
	}

	// First create a new map containing only the sites and their inferred values that we would
	// like to export.
	exported := make(map[primitiveSite]InferredVal)
	sitesToExport := i.chooseSitesToExport()
	for site, val := range i.mapping {

		if !sitesToExport[site] {
			continue
		}

		if upstreamVal, upstreamPresent := i.upstreamMapping[site]; upstreamPresent {
			diff, diffNonempty := inferredValDiff(val, upstreamVal)
			if diffNonempty && diff != nil {
				exported[site] = diff
			}
		} else {
			exported[site] = val
		}
	}

	if len(exported) > 0 {
		m := newInferredMap()
		m.mapping = exported
		pass.ExportPackageFact(m)
	}
}

// GobEncode encodes the inferred map via gob encoding.
func (i *InferredMap) GobEncode() ([]byte, error) {
	// Then, just encode the slim version of the map.
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(i.mapping); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// GobDecode decodes the InferredMap from buffer.
func (i *InferredMap) GobDecode(input []byte) error {
	buf := bytes.NewBuffer(input)
	dec := gob.NewDecoder(buf)

	i.mapping, i.upstreamMapping = make(map[primitiveSite]InferredVal), make(map[primitiveSite]InferredVal)
	if err := dec.Decode(&i.mapping); err != nil {
		return err
	}

	return nil
}

// chooseSitesToExport returns the set of AnnotationSites mapped by this InferredMap that are both
// reachable from and that reach an Exported (in the go sense; i.e. capitalized) site. We define
// reachability  here to be reflexive, and we choose this definition so that the returned set is
// convex -guaranteeing that we never forget a semantically meaningful implication - yet minimal -
// containing no site that could be forgotten without sacrificing soundness
func (i *InferredMap) chooseSitesToExport() map[primitiveSite]bool {
	toExport := make(map[primitiveSite]bool)
	reachableFromExported := make(map[primitiveSite]bool)
	reachesExported := make(map[primitiveSite]bool)

	var markReachableFromExported func(site primitiveSite)
	markReachableFromExported = func(site primitiveSite) {
		if val, isUndetermined := i.mapping[site].(*UndeterminedVal); isUndetermined && !site.Exported && !toExport[site] && !reachableFromExported[site] {
			if reachesExported[site] {
				toExport[site] = true
			} else {
				reachableFromExported[site] = true
			}

			for implicate := range val.Implicates {
				markReachableFromExported(implicate)
			}
		}
	}

	var markReachesExported func(site primitiveSite)
	markReachesExported = func(site primitiveSite) {
		if val, isUndetermined := i.mapping[site].(*UndeterminedVal); isUndetermined && !site.Exported && !toExport[site] && !reachesExported[site] {
			if reachableFromExported[site] {
				toExport[site] = true
			} else {
				reachesExported[site] = true
			}

			for implicant := range val.Implicants {
				markReachesExported(implicant)
			}
		}
	}

	for site := range i.mapping {
		if !site.Exported {
			continue
		}
		// Mark the current site as to be exported.
		toExport[site] = true

		// For UndeterminedVal, we visit the implicants and implicates recursively and mark
		// them as to be exported as well.
		if val, ok := i.mapping[site].(*UndeterminedVal); ok {
			for implicant := range val.Implicants {
				markReachesExported(implicant)
			}
			for implicate := range val.Implicates {
				markReachableFromExported(implicate)
			}
		}
	}
	return toExport
}

// The following method implementations make InferredMap satisfy the annotation.Map
// interface, so that triggers can be checked against it.

// CheckFieldAnn checks this InferredMap for a concrete mapping of the field key provided
func (i *InferredMap) CheckFieldAnn(fld *types.Var) (annotation.Val, bool) {
	return i.checkAnnotationKey(annotation.FieldAnnotationKey{FieldDecl: fld})
}

// CheckFuncParamAnn checks this InferredMap for a concrete mapping of the param key provided
func (i *InferredMap) CheckFuncParamAnn(fdecl *types.Func, num int) (annotation.Val, bool) {
	return i.checkAnnotationKey(annotation.ParamKeyFromArgNum(fdecl, num))
}

// CheckFuncRetAnn checks this InferredMap for a concrete mapping of the return key provided
func (i *InferredMap) CheckFuncRetAnn(fdecl *types.Func, num int) (annotation.Val, bool) {
	return i.checkAnnotationKey(annotation.RetKeyFromRetNum(fdecl, num))
}

// CheckFuncRecvAnn checks this InferredMap for a concrete mapping of the receiver key provided
func (i *InferredMap) CheckFuncRecvAnn(fdecl *types.Func) (annotation.Val, bool) {
	return i.checkAnnotationKey(annotation.RecvAnnotationKey{FuncDecl: fdecl})
}

// CheckDeepTypeAnn checks this InferredMap for a concrete mapping of the type name key provideed
func (i *InferredMap) CheckDeepTypeAnn(name *types.TypeName) (annotation.Val, bool) {
	return i.checkAnnotationKey(annotation.TypeNameAnnotationKey{TypeDecl: name})
}

// CheckGlobalVarAnn checks this InferredMap for a concrete mapping of the global variable key provided
func (i *InferredMap) CheckGlobalVarAnn(v *types.Var) (annotation.Val, bool) {
	return i.checkAnnotationKey(annotation.GlobalVarAnnotationKey{VarDecl: v})
}

func (i *InferredMap) checkAnnotationKey(key annotation.Key) (annotation.Val, bool) {
	shallowKey := newPrimitiveSite(key, false)
	deepKey := newPrimitiveSite(key, true)

	shallowVal, shallowOk := i.mapping[shallowKey]
	deepVal, deepOk := i.mapping[deepKey]
	if !shallowOk || !deepOk {
		return annotation.EmptyVal, false
	}

	shallowBoolVal, shallowOk := shallowVal.(*DeterminedVal)
	deepBoolVal, deepOk := deepVal.(*DeterminedVal)
	if !shallowOk || !deepOk {
		return annotation.EmptyVal, false
	}

	return annotation.Val{
		IsNilable:        shallowBoolVal.Bool.Val(),
		IsDeepNilable:    deepBoolVal.Bool.Val(),
		IsNilableSet:     true,
		IsDeepNilableSet: true,
	}, true
}
