// Copyright 2015 The Vanadium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reader

import (
	"flag"
	"path/filepath"
	"strings"

	"v.io/jiri/jiri"
)

// RegisterReaderFlags registers the flags commonly used with a profiles.Reader.
// --profiles-manifest, --skip-profiles, --profiles and --merge-policies.
func RegisterReaderFlags(flags *flag.FlagSet, profilesMode *ProfilesMode, manifest, profiles *string, defaultManifest string, policies *MergePolicies) {
	flags.Var(profilesMode, "skip-profiles", "if set, no profiles will be used")
	registerProfilesFlag(flags, profiles)
	registerMergePoliciesFlag(flags, policies)
	registerManifestFlag(flags, manifest, defaultManifest)
}

// RegisterProfilesFlag registers the --profiles flag
func registerProfilesFlag(flags *flag.FlagSet, profiles *string) {
	flags.StringVar(profiles, "profiles", "base,jiri", "a comma separated list of profiles to use")
}

// RegisterMergePoliciesFlag registers the --merge-policies flag
func registerMergePoliciesFlag(flags *flag.FlagSet, policies *MergePolicies) {
	flags.Var(policies, "merge-policies", "specify policies for merging environment variables")
}

// RegisterManifestFlag registers the commonly used --profiles-manifest
// flag with the supplied FlagSet.
func registerManifestFlag(flags *flag.FlagSet, manifest *string, defaultManifest string) {
	root := jiri.FindRoot()
	flags.StringVar(manifest, "profiles-manifest", filepath.Join(root, defaultManifest), "specify the profiles XML manifest filename.")
	flags.Lookup("profiles-manifest").DefValue = filepath.Join("$JIRI_ROOT", defaultManifest)
}

type AppendJiriProfileMode bool

const (
	AppendJiriProfile      AppendJiriProfileMode = true
	DoNotAppendJiriProfile                       = false
)

// InitProfilesFromFlag splits a comma separated list of profile names into
// a slice and optionally appends the 'jiri' profile if it's not already
// present.
func InitProfilesFromFlag(flag string, appendJiriProfile AppendJiriProfileMode) []string {
	n := strings.Split(flag, ",")
	if appendJiriProfile == AppendJiriProfile && !strings.Contains(flag, "jiri") {
		n = append(n, "jiri")
	}
	return n
}
