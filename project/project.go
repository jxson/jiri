// Copyright 2015 The Vanadium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package project

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fuchsia.googlesource.com/jiri"
	"fuchsia.googlesource.com/jiri/collect"
	"fuchsia.googlesource.com/jiri/gitutil"
	"fuchsia.googlesource.com/jiri/googlesource"
	"fuchsia.googlesource.com/jiri/runutil"
)

var JiriProject = "release.go.jiri"
var JiriName = "jiri"
var JiriPackage = "fuchsia.googlesource.com/jiri"

// CL represents a changelist.
type CL struct {
	// Author identifies the author of the changelist.
	Author string
	// Email identifies the author's email.
	Email string
	// Description holds the description of the changelist.
	Description string
}

// Manifest represents a setting used for updating the universe.
type Manifest struct {
	Imports      []Import      `xml:"imports>import"`
	LocalImports []LocalImport `xml:"imports>localimport"`
	Projects     []Project     `xml:"projects>project"`
	Tools        []Tool        `xml:"tools>tool"`
	// SnapshotPath is the relative path to the snapshot file from JIRI_ROOT.
	// It is only set when creating a snapshot.
	SnapshotPath string   `xml:"snapshotpath,attr,omitempty"`
	XMLName      struct{} `xml:"manifest"`
}

// ManifestFromBytes returns a manifest parsed from data, with defaults filled
// in.
func ManifestFromBytes(data []byte) (*Manifest, error) {
	m := new(Manifest)
	if err := xml.Unmarshal(data, m); err != nil {
		return nil, err
	}
	if err := m.fillDefaults(); err != nil {
		return nil, err
	}
	return m, nil
}

// ManifestFromFile returns a manifest parsed from the contents of filename,
// with defaults filled in.
//
// Note that unlike ProjectFromFile, ManifestFromFile does not convert project
// paths to absolute paths because it's possible to load a manifest with a
// specific root directory different from jirix.Root.  The usual way to load a
// manifest is through LoadManifest, which does absolutize the paths, and uses
// the correct root directory.
func ManifestFromFile(jirix *jiri.X, filename string) (*Manifest, error) {
	data, err := jirix.NewSeq().ReadFile(filename)
	if err != nil {
		return nil, err
	}
	m, err := ManifestFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest %s: %v", filename, err)
	}
	return m, nil
}

var (
	newlineBytes       = []byte("\n")
	emptyImportsBytes  = []byte("\n  <imports></imports>\n")
	emptyProjectsBytes = []byte("\n  <projects></projects>\n")
	emptyToolsBytes    = []byte("\n  <tools></tools>\n")

	endElemBytes        = []byte("/>\n")
	endImportBytes      = []byte("></import>\n")
	endLocalImportBytes = []byte("></localimport>\n")
	endProjectBytes     = []byte("></project>\n")
	endToolBytes        = []byte("></tool>\n")

	endImportSoloBytes  = []byte("></import>")
	endProjectSoloBytes = []byte("></project>")
	endElemSoloBytes    = []byte("/>")
)

// deepCopy returns a deep copy of Manifest.
func (m *Manifest) deepCopy() *Manifest {
	x := new(Manifest)
	x.SnapshotPath = m.SnapshotPath
	x.Imports = append([]Import(nil), m.Imports...)
	x.LocalImports = append([]LocalImport(nil), m.LocalImports...)
	x.Projects = append([]Project(nil), m.Projects...)
	x.Tools = append([]Tool(nil), m.Tools...)
	return x
}

// ToBytes returns m as serialized bytes, with defaults unfilled.
func (m *Manifest) ToBytes() ([]byte, error) {
	m = m.deepCopy() // avoid changing manifest when unfilling defaults.
	if err := m.unfillDefaults(); err != nil {
		return nil, err
	}
	data, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("manifest xml.Marshal failed: %v", err)
	}
	// It's hard (impossible?) to get xml.Marshal to elide some of the empty
	// elements, or produce short empty elements, so we post-process the data.
	data = bytes.Replace(data, emptyImportsBytes, newlineBytes, -1)
	data = bytes.Replace(data, emptyProjectsBytes, newlineBytes, -1)
	data = bytes.Replace(data, emptyToolsBytes, newlineBytes, -1)
	data = bytes.Replace(data, endImportBytes, endElemBytes, -1)
	data = bytes.Replace(data, endLocalImportBytes, endElemBytes, -1)
	data = bytes.Replace(data, endProjectBytes, endElemBytes, -1)
	data = bytes.Replace(data, endToolBytes, endElemBytes, -1)
	if !bytes.HasSuffix(data, newlineBytes) {
		data = append(data, '\n')
	}
	return data, nil
}

func safeWriteFile(jirix *jiri.X, filename string, data []byte) error {
	tmp := filename + ".tmp"
	return jirix.NewSeq().
		MkdirAll(filepath.Dir(filename), 0755).
		WriteFile(tmp, data, 0644).
		Rename(tmp, filename).
		Done()
}

// ToFile writes the manifest m to a file with the given filename, with
// defaults unfilled and all project paths relative to the jiri root.
func (m *Manifest) ToFile(jirix *jiri.X, filename string) error {
	// Replace absolute paths with relative paths to make it possible to move
	// the $JIRI_ROOT directory locally.
	projects := []Project{}
	for _, project := range m.Projects {
		if err := project.relativizePaths(jirix.Root); err != nil {
			return err
		}
		projects = append(projects, project)
	}
	m.Projects = projects
	data, err := m.ToBytes()
	if err != nil {
		return err
	}
	return safeWriteFile(jirix, filename, data)
}

func (m *Manifest) fillDefaults() error {
	for index := range m.Imports {
		if err := m.Imports[index].fillDefaults(); err != nil {
			return err
		}
	}
	for index := range m.LocalImports {
		if err := m.LocalImports[index].validate(); err != nil {
			return err
		}
	}
	for index := range m.Projects {
		if err := m.Projects[index].fillDefaults(); err != nil {
			return err
		}
	}
	for index := range m.Tools {
		if err := m.Tools[index].fillDefaults(); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manifest) unfillDefaults() error {
	for index := range m.Imports {
		if err := m.Imports[index].unfillDefaults(); err != nil {
			return err
		}
	}
	for index := range m.LocalImports {
		if err := m.LocalImports[index].validate(); err != nil {
			return err
		}
	}
	for index := range m.Projects {
		if err := m.Projects[index].unfillDefaults(); err != nil {
			return err
		}
	}
	for index := range m.Tools {
		if err := m.Tools[index].unfillDefaults(); err != nil {
			return err
		}
	}
	return nil
}

// Import represents a remote manifest import.
type Import struct {
	// Manifest file to use from the remote manifest project.
	Manifest string `xml:"manifest,attr,omitempty"`
	// Name is the name of the remote manifest project, used to determine the
	// project key.
	Name string `xml:"name,attr,omitempty"`
	// Remote is the remote manifest project to import.
	Remote string `xml:"remote,attr,omitempty"`
	// RemoteBranch is the name of the remote branch to track. It doesn't affect
	// the name of the local branch that jiri maintains, which is always
	// "master". If not set, "master" is used as the default.
	RemoteBranch string `xml:"remotebranch,attr,omitempty"`
	// Root path, prepended to all project paths specified in the manifest file.
	Root    string   `xml:"root,attr,omitempty"`
	XMLName struct{} `xml:"import"`
}

func (i *Import) fillDefaults() error {
	if i.RemoteBranch == "" {
		i.RemoteBranch = "master"
	}
	return i.validate()
}

func (i *Import) unfillDefaults() error {
	if i.RemoteBranch == "master" {
		i.RemoteBranch = ""
	}
	return i.validate()
}

func (i *Import) validate() error {
	if i.Manifest == "" || i.Remote == "" {
		return fmt.Errorf("bad import: both manifest and remote must be specified")
	}
	return nil
}

func (i *Import) toProject(path string) (Project, error) {
	p := Project{
		Name:         i.Name,
		Path:         path,
		Remote:       i.Remote,
		RemoteBranch: i.RemoteBranch,
	}
	err := p.fillDefaults()
	return p, err
}

// ProjectKey returns the unique ProjectKey for the imported project.
func (i *Import) ProjectKey() ProjectKey {
	return MakeProjectKey(i.Name, i.Remote)
}

// projectKeyFileName returns a file name based on the ProjectKey.
func (i *Import) projectKeyFileName() string {
	// TODO(toddw): Disallow weird characters from project names.
	hash := fnv.New64a()
	hash.Write([]byte(i.ProjectKey()))
	return fmt.Sprintf("%s_%x", i.Name, hash.Sum64())
}

// cycleKey returns a key based on the remote and manifest, used for
// cycle-detection.  It's only valid for new-style remote imports; it's empty
// for the old-style local imports.
func (i *Import) cycleKey() string {
	if i.Remote == "" {
		return ""
	}
	// We don't join the remote and manifest with a slash or any other url-safe
	// character, since that might not be unique.  E.g.
	//   remote:   https://foo.com/a/b    remote:   https://foo.com/a
	//   manifest: c                      manifest: b/c
	// In both cases, the key would be https://foo.com/a/b/c.
	return i.Remote + " + " + i.Manifest
}

// LocalImport represents a local manifest import.
type LocalImport struct {
	// Manifest file to import from.
	File    string   `xml:"file,attr,omitempty"`
	XMLName struct{} `xml:"localimport"`
}

func (i *LocalImport) validate() error {
	if i.File == "" {
		return fmt.Errorf("bad localimport: must specify file: %+v", *i)
	}
	return nil
}

// ProjectKey is a unique string for a project.
type ProjectKey string

// MakeProjectKey returns the project key, given the project name and remote.
func MakeProjectKey(name, remote string) ProjectKey {
	return ProjectKey(name + projectKeySeparator + remote)
}

// projectKeySeparator is a reserved string used in ProjectKeys.  It cannot
// occur in Project names.
const projectKeySeparator = "="

// ProjectKeys is a slice of ProjectKeys implementing the Sort interface.
type ProjectKeys []ProjectKey

func (pks ProjectKeys) Len() int           { return len(pks) }
func (pks ProjectKeys) Less(i, j int) bool { return string(pks[i]) < string(pks[j]) }
func (pks ProjectKeys) Swap(i, j int)      { pks[i], pks[j] = pks[j], pks[i] }

// Project represents a jiri project.
type Project struct {
	// Name is the project name.
	Name string `xml:"name,attr,omitempty"`
	// Path is the path used to store the project locally. Project
	// manifest uses paths that are relative to the $JIRI_ROOT
	// environment variable. When a manifest is parsed (e.g. in
	// RemoteProjects), the program logic converts the relative
	// paths to an absolute paths, using the current value of the
	// $JIRI_ROOT environment variable as a prefix.
	Path string `xml:"path,attr,omitempty"`
	// Remote is the project remote.
	Remote string `xml:"remote,attr,omitempty"`
	// RemoteBranch is the name of the remote branch to track.  It doesn't affect
	// the name of the local branch that jiri maintains, which is always "master".
	RemoteBranch string `xml:"remotebranch,attr,omitempty"`
	// Revision is the revision the project should be advanced to during "jiri
	// update".  If Revision is set, RemoteBranch will be ignored.  If Revision
	// is not set, "HEAD" is used as the default.
	Revision string `xml:"revision,attr,omitempty"`
	// GerritHost is the gerrit host where project CLs will be sent.
	GerritHost string `xml:"gerrithost,attr,omitempty"`
	// GitHooks is a directory containing git hooks that will be installed for
	// this project.
	GitHooks string `xml:"githooks,attr,omitempty"`
	// RunHook is a script that will run when the project is created, updated,
	// or moved.  The argument to the script will be "create", "update" or
	// "move" depending on the type of operation being performed.
	RunHook string   `xml:"runhook,attr,omitempty"`
	XMLName struct{} `xml:"project"`
}

// ProjectFromFile returns a project parsed from the contents of filename,
// with defaults filled in and all paths absolute.
func ProjectFromFile(jirix *jiri.X, filename string) (*Project, error) {
	data, err := jirix.NewSeq().ReadFile(filename)
	if err != nil {
		return nil, err
	}

	p := new(Project)
	if err := xml.Unmarshal(data, p); err != nil {
		return nil, err
	}
	if err := p.fillDefaults(); err != nil {
		return nil, err
	}
	p.absolutizePaths(jirix.Root)
	return p, nil
}

// ToFile writes the project p to a file with the given filename, with defaults
// unfilled and all paths relative to the jiri root.
func (p Project) ToFile(jirix *jiri.X, filename string) error {
	if err := p.unfillDefaults(); err != nil {
		return err
	}
	// Replace absolute paths with relative paths to make it possible to move
	// the $JIRI_ROOT directory locally.
	if err := p.relativizePaths(jirix.Root); err != nil {
		return err
	}
	data, err := xml.Marshal(p)
	if err != nil {
		return fmt.Errorf("project xml.Marshal failed: %v", err)
	}
	// Same logic as Manifest.ToBytes, to make the output more compact.
	data = bytes.Replace(data, endProjectSoloBytes, endElemSoloBytes, -1)
	if !bytes.HasSuffix(data, newlineBytes) {
		data = append(data, '\n')
	}
	return safeWriteFile(jirix, filename, data)
}

// absolutizePaths makes all relative paths absolute by prepending basepath.
func (p *Project) absolutizePaths(basepath string) {
	if p.Path != "" && !filepath.IsAbs(p.Path) {
		p.Path = filepath.Join(basepath, p.Path)
	}
	if p.GitHooks != "" && !filepath.IsAbs(p.GitHooks) {
		p.GitHooks = filepath.Join(basepath, p.GitHooks)
	}
	if p.RunHook != "" && !filepath.IsAbs(p.RunHook) {
		p.RunHook = filepath.Join(basepath, p.RunHook)
	}
}

// relativizePaths makes all absolute paths relative to basepath.
func (p *Project) relativizePaths(basepath string) error {
	if filepath.IsAbs(p.Path) {
		relPath, err := filepath.Rel(basepath, p.Path)
		if err != nil {
			return err
		}
		p.Path = relPath
	}
	if filepath.IsAbs(p.GitHooks) {
		relGitHooks, err := filepath.Rel(basepath, p.GitHooks)
		if err != nil {
			return err
		}
		p.GitHooks = relGitHooks
	}
	if filepath.IsAbs(p.RunHook) {
		relRunHook, err := filepath.Rel(basepath, p.RunHook)
		if err != nil {
			return err
		}
		p.RunHook = relRunHook
	}
	return nil
}

// Key returns the unique ProjectKey for the project.
func (p Project) Key() ProjectKey {
	return MakeProjectKey(p.Name, p.Remote)
}

func (p *Project) fillDefaults() error {
	if p.RemoteBranch == "" {
		p.RemoteBranch = "master"
	}
	if p.Revision == "" {
		p.Revision = "HEAD"
	}
	return p.validate()
}

func (p *Project) unfillDefaults() error {
	if p.RemoteBranch == "master" {
		p.RemoteBranch = ""
	}
	if p.Revision == "HEAD" {
		p.Revision = ""
	}
	return p.validate()
}

func (p *Project) validate() error {
	if strings.Contains(p.Name, projectKeySeparator) {
		return fmt.Errorf("bad project: name cannot contain %q: %+v", projectKeySeparator, *p)
	}
	return nil
}

// Projects maps ProjectKeys to Projects.
type Projects map[ProjectKey]Project

// toSlice returns a slice of Projects in the Projects map.
func (ps Projects) toSlice() []Project {
	var pSlice []Project
	for _, p := range ps {
		pSlice = append(pSlice, p)
	}
	return pSlice
}

// Find returns all projects in Projects with the given key or name.
func (ps Projects) Find(keyOrName string) Projects {
	projects := Projects{}
	if p, ok := ps[ProjectKey(keyOrName)]; ok {
		projects[ProjectKey(keyOrName)] = p
	} else {
		for key, p := range ps {
			if keyOrName == p.Name {
				projects[key] = p
			}
		}
	}
	return projects
}

// FindUnique returns the project in Projects with the given key or name, and
// returns an error if none or multiple matching projects are found.
func (ps Projects) FindUnique(keyOrName string) (Project, error) {
	var p Project
	projects := ps.Find(keyOrName)
	if len(projects) == 0 {
		return p, fmt.Errorf("no projects found with key or name %q", keyOrName)
	}
	if len(projects) > 1 {
		return p, fmt.Errorf("multiple projects found with name %q", keyOrName)
	}
	// Return the only project in projects.
	for _, project := range projects {
		p = project
	}
	return p, nil
}

// Tools maps jiri tool names, to their detailed description.
type Tools map[string]Tool

// toSlice returns a slice of Tools in the Tools map.
func (ts Tools) toSlice() []Tool {
	var tSlice []Tool
	for _, t := range ts {
		tSlice = append(tSlice, t)
	}
	return tSlice
}

// Tool represents a jiri tool.
type Tool struct {
	// Data is a relative path to a directory for storing tool data
	// (e.g. tool configuration files). The purpose of this field is to
	// decouple the configuration of the data directory from the tool
	// itself so that the location of the data directory can change
	// without the need to change the tool.
	Data string `xml:"data,attr,omitempty"`
	// Name is the name of the tool binary.
	Name string `xml:"name,attr,omitempty"`
	// Package is the package path of the tool.
	Package string `xml:"package,attr,omitempty"`
	// Project identifies the project that contains the tool. If not
	// set, "https://fuchsia.googlesource.com/<JiriProject>" is
	// used as the default.
	Project string   `xml:"project,attr,omitempty"`
	XMLName struct{} `xml:"tool"`
}

func (t *Tool) fillDefaults() error {
	if t.Data == "" {
		t.Data = "data"
	}
	if t.Project == "" {
		t.Project = "https://fuchsia.googlesource.com/" + JiriProject
	}
	return nil
}

func (t *Tool) unfillDefaults() error {
	if t.Data == "data" {
		t.Data = ""
	}
	// Don't unfill the jiri project setting, since that's not meant to be
	// optional.
	return nil
}

// ScanMode determines whether LocalProjects should scan the local filesystem
// for projects (FullScan), or optimistically assume that the local projects
// will match those in the manifest (FastScan).
type ScanMode bool

const (
	FastScan = ScanMode(false)
	FullScan = ScanMode(true)
)

// Update represents an update of projects as a map from
// project names to a collections of commits.
type Update map[string][]CL

// CreateSnapshot creates a manifest that encodes the current state of master
// branches of all projects and writes this snapshot out to the given file.
func CreateSnapshot(jirix *jiri.X, file, snapshotPath string) error {
	jirix.TimerPush("create snapshot")
	defer jirix.TimerPop()

	// If snapshotPath is empty, use the file as the path.
	if snapshotPath == "" {
		snapshotPath = file
	}

	// Get a clean, symlink-free, relative path to the snapshot.
	snapshotPath = filepath.Clean(snapshotPath)
	if evaledSnapshotPath, err := filepath.EvalSymlinks(snapshotPath); err == nil {
		snapshotPath = evaledSnapshotPath
	}
	if relSnapshotPath, err := filepath.Rel(jirix.Root, snapshotPath); err == nil {
		snapshotPath = relSnapshotPath
	}

	manifest := Manifest{
		SnapshotPath: snapshotPath,
	}

	// Add all local projects to manifest.
	localProjects, err := LocalProjects(jirix, FullScan)
	if err != nil {
		return err
	}
	for _, project := range localProjects {
		manifest.Projects = append(manifest.Projects, project)
	}

	// Add all tools from the current manifest to the snapshot manifest.
	// We can't just call LoadManifest here, since that determines the
	// local projects using FastScan, but if we're calling CreateSnapshot
	// during "jiri update" and we added some new projects, they won't be
	// found anymore.
	_, tools, err := loadManifestFile(jirix, jirix.JiriManifestFile(), localProjects)
	if err != nil {
		return err
	}
	for _, tool := range tools {
		manifest.Tools = append(manifest.Tools, tool)
	}
	return manifest.ToFile(jirix, file)
}

// CheckoutSnapshot updates project state to the state specified in the given
// snapshot file.  Note that the snapshot file must not contain remote imports.
func CheckoutSnapshot(jirix *jiri.X, snapshot string, gc bool) error {
	// Find all local projects.
	scanMode := FastScan
	if gc {
		scanMode = FullScan
	}
	localProjects, err := LocalProjects(jirix, scanMode)
	if err != nil {
		return err
	}
	remoteProjects, remoteTools, err := LoadSnapshotFile(jirix, snapshot)
	if err != nil {
		return err
	}
	if err := updateTo(jirix, localProjects, remoteProjects, remoteTools, gc); err != nil {
		return err
	}
	return WriteUpdateHistorySnapshot(jirix, snapshot)
}

// LoadSnapshotFile loads the specified snapshot manifest.  If the snapshot
// manifest contains a remote import, an error will be returned.
func LoadSnapshotFile(jirix *jiri.X, file string) (Projects, Tools, error) {
	return loadManifestFile(jirix, file, nil)
}

// CurrentProjectKey gets the key of the current project from the current
// directory by reading the jiri project metadata located in a directory at the
// root of the current repository.
func CurrentProjectKey(jirix *jiri.X) (ProjectKey, error) {
	topLevel, err := gitutil.New(jirix.NewSeq()).TopLevel()
	if err != nil {
		return "", nil
	}
	metadataDir := filepath.Join(topLevel, jiri.ProjectMetaDir)
	if _, err := jirix.NewSeq().Stat(metadataDir); err == nil {
		project, err := ProjectFromFile(jirix, filepath.Join(metadataDir, jiri.ProjectMetaFile))
		if err != nil {
			return "", err
		}
		return project.Key(), nil
	}
	return "", nil
}

// setProjectRevisions sets the current project revision from the master for
// each project as found on the filesystem
func setProjectRevisions(jirix *jiri.X, projects Projects) (_ Projects, e error) {
	for name, project := range projects {
		revision, err := gitutil.New(jirix.NewSeq(), gitutil.RootDirOpt(project.Path)).CurrentRevisionOfBranch("master")
		if err != nil {
			return nil, err
		}
		project.Revision = revision
		projects[name] = project
	}
	return projects, nil
}

// LocalProjects returns projects on the local filesystem.  If all projects in
// the manifest exist locally and scanMode is set to FastScan, then only the
// projects in the manifest that exist locally will be returned.  Otherwise, a
// full scan of the filesystem will take place, and all found projects will be
// returned.
func LocalProjects(jirix *jiri.X, scanMode ScanMode) (Projects, error) {
	jirix.TimerPush("local projects")
	defer jirix.TimerPop()

	latestSnapshot := jirix.UpdateHistoryLatestLink()
	latestSnapshotExists, err := jirix.NewSeq().IsFile(latestSnapshot)
	if err != nil {
		return nil, err
	}
	if scanMode == FastScan && latestSnapshotExists {
		// Fast path: Full scan was not requested, and we have a snapshot containing
		// the latest update.  Check that the projects listed in the snapshot exist
		// locally.  If not, then fall back on the slow path.
		//
		// An error will be returned if the snapshot contains remote imports, since
		// that would cause an infinite loop; we'd need local projects, in order to
		// load the snapshot, in order to determine the local projects.
		snapshotProjects, _, err := LoadSnapshotFile(jirix, latestSnapshot)
		if err != nil {
			return nil, err
		}
		projectsExist, err := projectsExistLocally(jirix, snapshotProjects)
		if err != nil {
			return nil, err
		}
		if projectsExist {
			return setProjectRevisions(jirix, snapshotProjects)
		}
	}

	// Slow path: Either full scan was requested, or projects exist in manifest
	// that were not found locally.  Do a recursive scan of all projects under
	// JIRI_ROOT.
	projects := Projects{}
	jirix.TimerPush("scan fs")
	err = findLocalProjects(jirix, jirix.Root, projects)
	jirix.TimerPop()
	if err != nil {
		return nil, err
	}
	return setProjectRevisions(jirix, projects)
}

// projectsExistLocally returns true iff all the given projects exist on the
// local filesystem.
// Note that this may return true even if there are projects on the local
// filesystem not included in the provided projects argument.
func projectsExistLocally(jirix *jiri.X, projects Projects) (bool, error) {
	jirix.TimerPush("match manifest")
	defer jirix.TimerPop()
	for _, p := range projects {
		isLocal, err := isLocalProject(jirix, p.Path)
		if err != nil {
			return false, err
		}
		if !isLocal {
			return false, nil
		}
	}
	return true, nil
}

// PollProjects returns the set of changelists that exist remotely but not
// locally. Changes are grouped by projects and contain author identification
// and a description of their content.
func PollProjects(jirix *jiri.X, projectSet map[string]struct{}) (_ Update, e error) {
	jirix.TimerPush("poll projects")
	defer jirix.TimerPop()

	// Switch back to current working directory when we're done.
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	defer collect.Error(func() error { return jirix.NewSeq().Chdir(cwd).Done() }, &e)

	// Gather local & remote project data.
	localProjects, err := LocalProjects(jirix, FastScan)
	if err != nil {
		return nil, err
	}
	remoteProjects, _, err := LoadManifest(jirix)
	if err != nil {
		return nil, err
	}

	// Compute difference between local and remote.
	update := Update{}
	ops := computeOperations(localProjects, remoteProjects, false)
	s := jirix.NewSeq()
	for _, op := range ops {
		name := op.Project().Name

		// If given a project set, limit our results to those projects in the set.
		if len(projectSet) > 0 {
			if _, ok := projectSet[name]; !ok {
				continue
			}
		}

		// We only inspect this project if an update operation is required.
		cls := []CL{}
		if updateOp, ok := op.(updateOperation); ok {
			// Enter project directory - this assumes absolute paths.
			if err := s.Chdir(updateOp.destination).Done(); err != nil {
				return nil, err
			}

			// Fetch the latest from origin.
			if err := gitutil.New(jirix.NewSeq()).FetchRefspec("origin", updateOp.project.RemoteBranch); err != nil {
				return nil, err
			}

			// Collect commits visible from FETCH_HEAD that aren't visible from master.
			commitsText, err := gitutil.New(jirix.NewSeq()).Log("FETCH_HEAD", "master", "%an%n%ae%n%B")
			if err != nil {
				return nil, err
			}

			// Format those commits and add them to the results.
			for _, commitText := range commitsText {
				if got, want := len(commitText), 3; got < want {
					return nil, fmt.Errorf("Unexpected length of %v: got %v, want at least %v", commitText, got, want)
				}
				cls = append(cls, CL{
					Author:      commitText[0],
					Email:       commitText[1],
					Description: strings.Join(commitText[2:], "\n"),
				})
			}
		}
		update[name] = cls
	}
	return update, nil
}

// LoadManifest loads the manifest, starting with the .jiri_manifest file,
// resolving remote and local imports.  Returns the projects and tools specified
// by the manifest.
//
// WARNING: LoadManifest cannot be run multiple times in parallel!  It invokes
// git operations which require a lock on the filesystem.  If you see errors
// about ".git/index.lock exists", you are likely calling LoadManifest in
// parallel.
func LoadManifest(jirix *jiri.X) (Projects, Tools, error) {
	jirix.TimerPush("load manifest")
	defer jirix.TimerPop()
	file := jirix.JiriManifestFile()
	localProjects, err := LocalProjects(jirix, FastScan)
	if err != nil {
		return nil, nil, err
	}
	return loadManifestFile(jirix, file, localProjects)
}

// loadManifestFile loads the manifest starting with the given file, resolving
// remote and local imports.  Local projects are used to resolve remote imports;
// if nil, encountering any remote import will result in an error.
//
// WARNING: loadManifestFile cannot be run multiple times in parallel!  It
// invokes git operations which require a lock on the filesystem.  If you see
// errors about ".git/index.lock exists", you are likely calling
// loadManifestFile in parallel.
func loadManifestFile(jirix *jiri.X, file string, localProjects Projects) (Projects, Tools, error) {
	ld := newManifestLoader(localProjects, false)
	if err := ld.Load(jirix, "", file, ""); err != nil {
		return nil, nil, err
	}
	return ld.Projects, ld.Tools, nil
}

// getManifestRemote returns the remote url of the origin from the manifest
// repo.
// TODO(nlacasse,toddw): Once the manifest project is specified in the
// manifest, we should get the remote directly from the manifest, and not from
// the filesystem.
func getManifestRemote(jirix *jiri.X, manifestPath string) (string, error) {
	var remote string
	return remote, jirix.NewSeq().Pushd(manifestPath).Call(
		func() (e error) {
			remote, e = gitutil.New(jirix.NewSeq()).RemoteUrl("origin")
			return
		}, "get manifest origin").Done()
}

func loadUpdatedManifest(jirix *jiri.X, localProjects Projects) (Projects, Tools, string, error) {
	jirix.TimerPush("load updated manifest")
	defer jirix.TimerPop()
	ld := newManifestLoader(localProjects, true)
	if err := ld.Load(jirix, "", jirix.JiriManifestFile(), ""); err != nil {
		return nil, nil, ld.TmpDir, err
	}
	return ld.Projects, ld.Tools, ld.TmpDir, nil
}

// UpdateUniverse updates all local projects and tools to match the remote
// counterparts identified in the manifest. Optionally, the 'gc' flag can be
// used to indicate that local projects that no longer exist remotely should be
// removed.
func UpdateUniverse(jirix *jiri.X, gc bool) (e error) {
	jirix.TimerPush("update universe")
	defer jirix.TimerPop()

	// Find all local projects.
	scanMode := FastScan
	if gc {
		scanMode = FullScan
	}
	localProjects, err := LocalProjects(jirix, scanMode)
	if err != nil {
		return err
	}

	// Load the manifest, updating all manifest projects to match their remote
	// counterparts.
	s := jirix.NewSeq()
	remoteProjects, remoteTools, tmpLoadDir, err := loadUpdatedManifest(jirix, localProjects)
	if tmpLoadDir != "" {
		defer collect.Error(func() error { return s.RemoveAll(tmpLoadDir).Done() }, &e)
	}
	if err != nil {
		return err
	}
	return updateTo(jirix, localProjects, remoteProjects, remoteTools, gc)
}

// updateTo updates the local projects and tools to the state specified in
// remoteProjects and remoteTools.
func updateTo(jirix *jiri.X, localProjects, remoteProjects Projects, remoteTools Tools, gc bool) (e error) {
	s := jirix.NewSeq()
	// 1. Update all local projects to match the specified projects argument.
	if err := updateProjects(jirix, localProjects, remoteProjects, gc); err != nil {
		return err
	}
	// 2. Build all tools in a temporary directory.
	tmpToolsDir, err := s.TempDir("", "tmp-jiri-tools-build")
	if err != nil {
		return fmt.Errorf("TempDir() failed: %v", err)
	}
	defer collect.Error(func() error { return s.RemoveAll(tmpToolsDir).Done() }, &e)
	if err := buildToolsFromMaster(jirix, remoteProjects, remoteTools, tmpToolsDir); err != nil {
		return err
	}
	// 3. Install the tools into $JIRI_ROOT/.jiri_root/bin.
	if err := InstallTools(jirix, tmpToolsDir); err != nil {
		return err
	}
	// 4. If we have the jiri project, then update the jiri script in
	// $JIRI_ROOT/.jiri_root/scripts.
	jiriProject, err := remoteProjects.FindUnique(JiriProject)
	if err != nil {
		// jiri project not found.  This happens often in tests.  Ok to ignore.
		return nil
	}
	return updateJiriScript(jirix, jiriProject)
}

// WriteUpdateHistorySnapshot creates a snapshot of the current state of all
// projects and writes it to the update history directory.
func WriteUpdateHistorySnapshot(jirix *jiri.X, snapshotPath string) error {
	seq := jirix.NewSeq()
	snapshotFile := filepath.Join(jirix.UpdateHistoryDir(), time.Now().Format(time.RFC3339))
	if err := CreateSnapshot(jirix, snapshotFile, snapshotPath); err != nil {
		return err
	}

	latestLink, secondLatestLink := jirix.UpdateHistoryLatestLink(), jirix.UpdateHistorySecondLatestLink()

	// If the "latest" symlink exists, point the "second-latest" symlink to its value.
	latestLinkExists, err := seq.IsFile(latestLink)
	if err != nil {
		return err
	}
	if latestLinkExists {
		latestFile, err := os.Readlink(latestLink)
		if err != nil {
			return err
		}
		if err := seq.RemoveAll(secondLatestLink).Symlink(latestFile, secondLatestLink).Done(); err != nil {
			return err
		}
	}

	// Point the "latest" update history symlink to the new snapshot file.  Try
	// to keep the symlink relative, to make it easy to move or copy the entire
	// update_history directory.
	if rel, err := filepath.Rel(filepath.Dir(latestLink), snapshotFile); err == nil {
		snapshotFile = rel
	}
	return seq.RemoveAll(latestLink).Symlink(snapshotFile, latestLink).Done()
}

// ApplyToLocalMaster applies an operation expressed as the given function to
// the local master branch of the given projects.
func ApplyToLocalMaster(jirix *jiri.X, projects Projects, fn func() error) (e error) {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	defer collect.Error(func() error { return jirix.NewSeq().Chdir(cwd).Done() }, &e)

	s := jirix.NewSeq()
	git := gitutil.New(s)

	// Loop through all projects, checking out master and stashing any unstaged
	// changes.
	for _, project := range projects {
		p := project
		if err := s.Chdir(p.Path).Done(); err != nil {
			return err
		}
		branch, err := git.CurrentBranchName()
		if err != nil {
			return err
		}
		stashed, err := git.Stash()
		if err != nil {
			return err
		}
		if err := git.CheckoutBranch("master"); err != nil {
			return err
		}
		// After running the function, return to this project's directory,
		// checkout the original branch, and stash pop if necessary.
		defer collect.Error(func() error {
			if err := s.Chdir(p.Path).Done(); err != nil {
				return err
			}
			if err := git.CheckoutBranch(branch); err != nil {
				return err
			}
			if stashed {
				return git.StashPop()
			}
			return nil
		}, &e)
	}
	return fn()
}

// BuildTools builds the given tools and places the resulting binaries into the
// given directory.
func BuildTools(jirix *jiri.X, projects Projects, tools Tools, outputDir string) (e error) {
	jirix.TimerPush("build tools")
	defer jirix.TimerPop()
	if len(tools) == 0 {
		// Nothing to do here...
		return nil
	}
	toolPkgs := []string{}
	workspaceSet := map[string]bool{}
	for _, tool := range tools {
		toolPkgs = append(toolPkgs, tool.Package)
		toolProject, err := projects.FindUnique(tool.Project)
		if err != nil {
			return err
		}
		// Identify the Go workspace the tool is in. To this end we use a
		// heuristic that identifies the maximal suffix of the project path
		// that corresponds to a prefix of the package name.
		workspace := ""
		for i := 0; i < len(toolProject.Path); i++ {
			if toolProject.Path[i] == filepath.Separator {
				if strings.HasPrefix("src/"+tool.Package, filepath.ToSlash(toolProject.Path[i+1:])) {
					workspace = toolProject.Path[:i]
					break
				}
			}
		}
		if workspace == "" {
			return fmt.Errorf("could not identify go workspace for tool %v", tool.Name)
		}
		workspaceSet[workspace] = true
	}
	workspaces := []string{}
	for workspace := range workspaceSet {
		workspaces = append(workspaces, workspace)
	}
	if envGoPath := os.Getenv("GOPATH"); envGoPath != "" {
		workspaces = append(workspaces, strings.Split(envGoPath, string(filepath.ListSeparator))...)
	}
	s := jirix.NewSeq()
	// Put pkg files in a tempdir.  BuildTools uses the system go, and if
	// jiri-go uses a different go version than the system go, then you can get
	// weird errors when they share a pkgdir.
	tmpPkgDir, err := s.TempDir("", "tmp-pkg-dir")
	if err != nil {
		return fmt.Errorf("TempDir() failed: %v", err)
	}
	defer collect.Error(func() error { return jirix.NewSeq().RemoveAll(tmpPkgDir).Done() }, &e)

	// We unset GOARCH and GOOS because jiri update should always build for the
	// native architecture and OS.  Also, as of go1.5, setting GOBIN is not
	// compatible with GOARCH or GOOS.
	env := map[string]string{
		"GOARCH": "",
		"GOOS":   "",
		"GOBIN":  outputDir,
		"GOPATH": strings.Join(workspaces, string(filepath.ListSeparator)),
	}
	args := append([]string{"install", "-pkgdir", tmpPkgDir}, toolPkgs...)
	var stderr bytes.Buffer
	if err := s.Env(env).Capture(ioutil.Discard, &stderr).Last("go", args...); err != nil {
		return fmt.Errorf("tool build failed\n%v", stderr.String())
	}
	return nil
}

// buildToolsFromMaster builds and installs all jiri tools using the version
// available in the local master branch of the tools repository. Notably, this
// function does not perform any version control operation on the master
// branch.
func buildToolsFromMaster(jirix *jiri.X, projects Projects, tools Tools, outputDir string) error {
	toolsToBuild := Tools{}
	toolNames := []string{} // Used for logging purposes.
	for _, tool := range tools {
		// Skip tools with no package specified. Besides increasing
		// robustness, this step also allows us to create jiri root
		// fakes without having to provide an implementation for the "jiri"
		// tool, which every manifest needs to specify.
		if tool.Package == "" {
			continue
		}
		toolsToBuild[tool.Name] = tool
		toolNames = append(toolNames, tool.Name)
	}

	updateFn := func() error {
		return ApplyToLocalMaster(jirix, projects, func() error {
			return BuildTools(jirix, projects, toolsToBuild, outputDir)
		})
	}

	// Always log the output of updateFn, irrespective of the value of the
	// verbose flag.
	return jirix.NewSeq().Verbose(true).
		Call(updateFn, "build tools: %v", strings.Join(toolNames, " ")).
		Done()
}

// CleanupProjects restores the given jiri projects back to their master
// branches, resets to the specified revision if there is one, and gets rid of
// all the local changes. If "cleanupBranches" is true, it will also delete all
// the non-master branches.
func CleanupProjects(jirix *jiri.X, projects Projects, cleanupBranches bool) (e error) {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("Getwd() failed: %v", err)
	}
	defer collect.Error(func() error { return jirix.NewSeq().Chdir(wd).Done() }, &e)
	for _, project := range projects {
		if err := resetLocalProject(jirix, project, cleanupBranches); err != nil {
			return err
		}
	}
	return nil
}

// resetLocalProject checks out the master branch, cleans up untracked files
// and uncommitted changes, and optionally deletes all the other branches.
func resetLocalProject(jirix *jiri.X, project Project, cleanupBranches bool) error {
	git := gitutil.New(jirix.NewSeq())
	if err := jirix.NewSeq().Chdir(project.Path).Done(); err != nil {
		return err
	}
	// Check out master.
	curBranchName, err := git.CurrentBranchName()
	if err != nil {
		return err
	}
	if curBranchName != "master" {
		if err := git.CheckoutBranch("master", gitutil.ForceOpt(true)); err != nil {
			return err
		}
	}
	// Cleanup changes.
	if err := git.RemoveUntrackedFiles(); err != nil {
		return err
	}
	if err := resetProjectCurrentBranch(jirix, project); err != nil {
		return err
	}
	if !cleanupBranches {
		return nil
	}

	// Delete all the other branches.
	// At this point we should be at the master branch.
	branches, _, err := gitutil.New(jirix.NewSeq()).GetBranches()
	if err != nil {
		return err
	}
	for _, branch := range branches {
		if branch == "master" {
			continue
		}
		if err := git.DeleteBranch(branch, gitutil.ForceOpt(true)); err != nil {
			return err
		}
	}
	return nil
}

// isLocalProject returns true if there is a project at the given path.
func isLocalProject(jirix *jiri.X, path string) (bool, error) {
	// Existence of a metadata directory is how we know we've found a
	// Jiri-maintained project.
	metadataDir := filepath.Join(path, jiri.ProjectMetaDir)
	if _, err := jirix.NewSeq().Stat(metadataDir); err != nil {
		if runutil.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ProjectAtPath returns a Project struct corresponding to the project at the
// path in the filesystem.
func ProjectAtPath(jirix *jiri.X, path string) (Project, error) {
	metadataFile := filepath.Join(path, jiri.ProjectMetaDir, jiri.ProjectMetaFile)
	project, err := ProjectFromFile(jirix, metadataFile)
	if err != nil {
		return Project{}, err
	}
	return *project, nil
}

// findLocalProjects scans the filesystem for all projects.  Note that project
// directories can be nested recursively.
func findLocalProjects(jirix *jiri.X, path string, projects Projects) error {
	isLocal, err := isLocalProject(jirix, path)
	if err != nil {
		return err
	}
	if isLocal {
		project, err := ProjectAtPath(jirix, path)
		if err != nil {
			return err
		}
		if path != project.Path {
			return fmt.Errorf("project %v has path %v but was found in %v", project.Name, project.Path, path)
		}
		if p, ok := projects[project.Key()]; ok {
			return fmt.Errorf("name conflict: both %v and %v contain project with key %v", p.Path, project.Path, project.Key())
		}
		projects[project.Key()] = project
	}

	// Recurse into all the sub directories.
	fileInfos, err := jirix.NewSeq().ReadDir(path)
	if err != nil {
		return err
	}
	for _, fileInfo := range fileInfos {
		if fileInfo.IsDir() && !strings.HasPrefix(fileInfo.Name(), ".") {
			if err := findLocalProjects(jirix, filepath.Join(path, fileInfo.Name()), projects); err != nil {
				return err
			}
		}
	}
	return nil
}

// InstallTools installs the tools from the given directory into
// $JIRI_ROOT/.jiri_root/bin.
func InstallTools(jirix *jiri.X, dir string) error {
	jirix.TimerPush("install tools")
	defer jirix.TimerPop()
	fis, err := ioutil.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("ReadDir(%v) failed: %v", dir, err)
	}
	binDir := jirix.BinDir()
	if err := jirix.NewSeq().MkdirAll(binDir, 0755).Done(); err != nil {
		return fmt.Errorf("MkdirAll(%v) failed: %v", binDir, err)
	}
	s := jirix.NewSeq()
	for _, fi := range fis {
		installFn := func() error {
			src := filepath.Join(dir, fi.Name())
			dst := filepath.Join(binDir, fi.Name())
			return jirix.NewSeq().Rename(src, dst).Done()
		}
		if err := s.Verbose(true).Call(installFn, "install tool %q", fi.Name()).Done(); err != nil {
			return fmt.Errorf("error installing tool %q: %v", fi.Name(), err)
		}
	}
	return nil
}

// updateJiriScript copies the scripts/jiri script from the jiri repo to
// JIRI_ROOT/.jiri_root/scripts/jiri.
func updateJiriScript(jirix *jiri.X, jiriProject Project) error {
	s := jirix.NewSeq()
	updateFn := func() error {
		return ApplyToLocalMaster(jirix, Projects{jiriProject.Key(): jiriProject}, func() error {
			newJiriScriptPath := filepath.Join(jiriProject.Path, "scripts", "jiri")
			newJiriScript, err := s.Open(newJiriScriptPath)
			if err != nil {
				return err
			}
			s.MkdirAll(jirix.ScriptsDir(), 0755)
			jiriScriptOutPath := filepath.Join(jirix.ScriptsDir(), "jiri")
			jiriScriptOut, err := s.Create(jiriScriptOutPath)
			if err != nil {
				return err
			}
			if _, err := s.Copy(jiriScriptOut, newJiriScript); err != nil {
				return err
			}
			if err := s.Chmod(jiriScriptOutPath, 0750).Done(); err != nil {
				return err
			}

			return nil
		})
	}
	return jirix.NewSeq().Verbose(true).Call(updateFn, "update jiri script").Done()
}

// fetchProject fetches from the project remote.
func fetchProject(jirix *jiri.X, project Project) error {
	if project.Remote == "" {
		return fmt.Errorf("project %q does not have a remote", project.Name)
	}
	if err := gitutil.New(jirix.NewSeq()).SetRemoteUrl("origin", project.Remote); err != nil {
		return err
	}
	return gitutil.New(jirix.NewSeq()).Fetch("origin")
}

// resetProjectCurrentBranch resets the current branch to the revision and
// branch specified on the project.
func resetProjectCurrentBranch(jirix *jiri.X, project Project) error {
	if err := project.fillDefaults(); err != nil {
		return err
	}
	// Having a specific revision trumps everything else.
	if project.Revision != "HEAD" {
		return gitutil.New(jirix.NewSeq()).Reset(project.Revision)
	}
	// If no revision, reset to the configured remote branch.
	return gitutil.New(jirix.NewSeq()).Reset("origin/" + project.RemoteBranch)
}

// syncProjectMaster fetches from the project remote and resets the local master
// branch to the revision and branch specified on the project.
func syncProjectMaster(jirix *jiri.X, project Project) error {
	return ApplyToLocalMaster(jirix, Projects{project.Key(): project}, func() error {
		if err := fetchProject(jirix, project); err != nil {
			return err
		}
		return resetProjectCurrentBranch(jirix, project)
	})
}

// newManifestLoader returns a new manifest loader.  The localProjects are used
// to resolve remote imports; if nil, encountering any remote import will result
// in an error.  If update is true, remote manifest import projects that don't
// exist locally are cloned under TmpDir, and inserted into localProjects.
//
// If update is true, remote changes to manifest projects will be fetched, and
// manifest projects that don't exist locally will be created in temporary
// directories, and added to localProjects.
func newManifestLoader(localProjects Projects, update bool) *loader {
	return &loader{
		Projects:      make(Projects),
		Tools:         make(Tools),
		localProjects: localProjects,
		update:        update,
	}
}

type loader struct {
	Projects      Projects
	Tools         Tools
	TmpDir        string
	localProjects Projects
	update        bool
	cycleStack    []cycleInfo
}

type cycleInfo struct {
	file, key string
}

// loadNoCycles checks for cycles in imports.  There are two types of cycles:
//   file - Cycle in the paths of manifest files in the local filesystem.
//   key  - Cycle in the remote manifests specified by remote imports.
//
// Example of file cycles.  File A imports file B, and vice versa.
//     file=manifest/A              file=manifest/B
//     <manifest>                   <manifest>
//       <localimport file="B"/>      <localimport file="A"/>
//     </manifest>                  </manifest>
//
// Example of key cycles.  The key consists of "remote/manifest", e.g.
//   https://vanadium.googlesource.com/manifest/v2/default
// In the example, key x/A imports y/B, and vice versa.
//     key=x/A                               key=y/B
//     <manifest>                            <manifest>
//       <import remote="y" manifest="B"/>     <import remote="x" manifest="A"/>
//     </manifest>                           </manifest>
//
// The above examples are simple, but the general strategy is demonstrated.  We
// keep a single stack for both files and keys, and push onto each stack before
// running the recursive read or update function, and pop the stack when the
// function is done.  If we see a duplicate on the stack at any point, we know
// there's a cycle.  Note that we know the file for both local and remote
// imports, but we only know the key for remote imports; the key for local
// imports is empty.
//
// A more complex case would involve a combination of local and remote imports,
// using the "root" attribute to change paths on the local filesystem.  In this
// case the key will eventually expose the cycle.
func (ld *loader) loadNoCycles(jirix *jiri.X, root, file, cycleKey string) error {
	info := cycleInfo{file, cycleKey}
	for _, c := range ld.cycleStack {
		switch {
		case file == c.file:
			return fmt.Errorf("import cycle detected in local manifest files: %q", append(ld.cycleStack, info))
		case cycleKey == c.key && cycleKey != "":
			return fmt.Errorf("import cycle detected in remote manifest imports: %q", append(ld.cycleStack, info))
		}
	}
	ld.cycleStack = append(ld.cycleStack, info)
	if err := ld.load(jirix, root, file); err != nil {
		return err
	}
	ld.cycleStack = ld.cycleStack[:len(ld.cycleStack)-1]
	return nil
}

// shortFileName returns the relative path if file is relative to root,
// otherwise returns the file name unchanged.
func shortFileName(root, file string) string {
	if p := root + string(filepath.Separator); strings.HasPrefix(file, p) {
		return file[len(p):]
	}
	return file
}

func (ld *loader) Load(jirix *jiri.X, root, file, cycleKey string) error {
	jirix.TimerPush("load " + shortFileName(jirix.Root, file))
	defer jirix.TimerPop()
	return ld.loadNoCycles(jirix, root, file, cycleKey)
}

func (ld *loader) load(jirix *jiri.X, root, file string) error {
	m, err := ManifestFromFile(jirix, file)
	if err != nil {
		return err
	}
	// Process remote imports.
	for _, remote := range m.Imports {
		nextRoot := filepath.Join(root, remote.Root)
		remote.Name = filepath.Join(nextRoot, remote.Name)
		key := remote.ProjectKey()
		p, ok := ld.localProjects[key]
		if !ok {
			if !ld.update {
				return fmt.Errorf("can't resolve remote import: project %q not found locally", key)
			}
			// The remote manifest project doesn't exist locally.  Clone it into a
			// temp directory, and add it to ld.localProjects.
			if ld.TmpDir == "" {
				if ld.TmpDir, err = jirix.NewSeq().TempDir("", "jiri-load"); err != nil {
					return fmt.Errorf("TempDir() failed: %v", err)
				}
			}
			path := filepath.Join(ld.TmpDir, remote.projectKeyFileName())
			if p, err = remote.toProject(path); err != nil {
				return err
			}
			if err := jirix.NewSeq().MkdirAll(path, 0755).Done(); err != nil {
				return err
			}
			if err := gitutil.New(jirix.NewSeq()).Clone(p.Remote, path); err != nil {
				return err
			}
			ld.localProjects[key] = p
		}
		// Reset the project to its specified branch and load the next file.  Note
		// that we call load() recursively, so multiple files may be loaded by
		// resetAndLoad.
		p.Revision = "HEAD"
		p.RemoteBranch = remote.RemoteBranch
		nextFile := filepath.Join(p.Path, remote.Manifest)
		if err := ld.resetAndLoad(jirix, nextRoot, nextFile, remote.cycleKey(), p); err != nil {
			return err
		}
	}
	// Process local imports.
	for _, local := range m.LocalImports {
		// TODO(toddw): Add our invariant check that the file is in the same
		// repository as the current remote import repository.
		nextFile := filepath.Join(filepath.Dir(file), local.File)
		if err := ld.Load(jirix, root, nextFile, ""); err != nil {
			return err
		}
	}
	// Collect projects.
	for _, project := range m.Projects {
		// Make paths absolute by prepending JIRI_ROOT/<root>.
		project.absolutizePaths(filepath.Join(jirix.Root, root))
		// Prepend the root to the project name.  This will be a noop if the import is not rooted.
		project.Name = filepath.Join(root, project.Name)
		key := project.Key()
		if dup, ok := ld.Projects[key]; ok && dup != project {
			// TODO(toddw): Tell the user the other conflicting file.
			return fmt.Errorf("duplicate project %q found in %v", key, shortFileName(jirix.Root, file))
		}
		ld.Projects[key] = project
	}
	// Collect tools.
	for _, tool := range m.Tools {
		name := tool.Name
		if dup, ok := ld.Tools[name]; ok && dup != tool {
			// TODO(toddw): Tell the user the other conflicting file.
			return fmt.Errorf("duplicate tool %q found in %v", name, shortFileName(jirix.Root, file))
		}
		ld.Tools[name] = tool
	}
	return nil
}

func (ld *loader) resetAndLoad(jirix *jiri.X, root, file, cycleKey string, project Project) (e error) {
	// Change to the project.Path directory, and revert when done.
	pushd := jirix.NewSeq().Pushd(project.Path)
	defer collect.Error(pushd.Done, &e)
	// Reset the local master branch to what's specified on the project.  We only
	// fetch on updates; non-updates just perform the reset.
	//
	// TODO(toddw): Support "jiri update -local=p1,p2" by simply calling ld.Load
	// for the given projects, rather than ApplyToLocalMaster(fetch+reset+load).
	return ApplyToLocalMaster(jirix, Projects{project.Key(): project}, func() error {
		if ld.update {
			if err := fetchProject(jirix, project); err != nil {
				return err
			}
		}
		if err := resetProjectCurrentBranch(jirix, project); err != nil {
			return err
		}
		return ld.Load(jirix, root, file, cycleKey)
	})
}

// reportNonMaster checks if the given project is on master branch and
// if not, reports this fact along with information on how to update it.
func reportNonMaster(jirix *jiri.X, project Project) (e error) {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	defer collect.Error(func() error { return jirix.NewSeq().Chdir(cwd).Done() }, &e)
	s := jirix.NewSeq()
	if err := s.Chdir(project.Path).Done(); err != nil {
		return err
	}
	current, err := gitutil.New(jirix.NewSeq()).CurrentBranchName()
	if err != nil {
		return err
	}
	if current != "master" {
		line1 := fmt.Sprintf(`NOTE: "jiri update" only updates the "master" branch and the current branch is %q`, current)
		line2 := fmt.Sprintf(`to update the %q branch once the master branch is updated, run "git merge master"`, current)
		s.Verbose(true).Output([]string{line1, line2})
	}
	return nil
}

// groupByGoogleSourceHosts returns a map of googlesource host to a Projects
// map where all project remotes come from that host.
func groupByGoogleSourceHosts(ps Projects) map[string]Projects {
	m := make(map[string]Projects)
	for _, p := range ps {
		if !googlesource.IsGoogleSourceRemote(p.Remote) {
			continue
		}
		u, err := url.Parse(p.Remote)
		if err != nil {
			continue
		}
		host := u.Scheme + "://" + u.Host
		if _, ok := m[host]; !ok {
			m[host] = Projects{}
		}
		m[host][p.Key()] = p
	}
	return m
}

// getRemoteHeadRevisions attempts to get the repo statuses from remote for
// projects at HEAD so we can detect when a local project is already
// up-to-date.
func getRemoteHeadRevisions(jirix *jiri.X, remoteProjects Projects) {
	projectsAtHead := Projects{}
	for _, rp := range remoteProjects {
		if rp.Revision == "HEAD" {
			projectsAtHead[rp.Key()] = rp
		}
	}
	gsHostsMap := groupByGoogleSourceHosts(projectsAtHead)
	for host, projects := range gsHostsMap {
		// Create a slice of branch names, with duplicates removed.
		branchesMap := make(map[string]bool)
		for _, p := range projects {
			branchesMap[p.RemoteBranch] = true
		}
		branches := make([]string, 0, len(branchesMap))
		for b, _ := range branchesMap {
			branches = append(branches, b)
		}

		repoStatuses, err := googlesource.GetRepoStatuses(jirix, host, branches)
		if err != nil {
			// Log the error but don't fail.
			fmt.Fprintf(jirix.Stderr(), "Error fetching repo statuses from remote: %v\n", err)
			continue
		}
		for _, p := range projects {
			status, ok := repoStatuses[p.Name]
			if !ok {
				continue
			}
			rev, ok := status.Branches[p.RemoteBranch]
			if !ok || rev == "" {
				continue
			}
			rp := remoteProjects[p.Key()]
			rp.Revision = rev
			remoteProjects[p.Key()] = rp
		}
	}
}

func updateProjects(jirix *jiri.X, localProjects, remoteProjects Projects, gc bool) error {
	jirix.TimerPush("update projects")
	defer jirix.TimerPop()

	getRemoteHeadRevisions(jirix, remoteProjects)
	ops := computeOperations(localProjects, remoteProjects, gc)
	updates := newFsUpdates()
	for _, op := range ops {
		if err := op.Test(jirix, updates); err != nil {
			return err
		}
	}
	s := jirix.NewSeq()
	for _, op := range ops {
		updateFn := func() error { return op.Run(jirix) }
		// Always log the output of updateFn, irrespective of
		// the value of the verbose flag.
		if err := s.Verbose(true).Call(updateFn, "%v", op).Done(); err != nil {
			return fmt.Errorf("error updating project %q: %v", op.Project().Name, err)
		}
	}
	if err := runHooks(jirix, ops); err != nil {
		return err
	}
	return applyGitHooks(jirix, ops)
}

// runHooks runs all hooks for the given operations.
func runHooks(jirix *jiri.X, ops []operation) error {
	jirix.TimerPush("run hooks")
	defer jirix.TimerPop()
	for _, op := range ops {
		if op.Project().RunHook == "" {
			continue
		}
		if op.Kind() != "create" && op.Kind() != "move" && op.Kind() != "update" {
			continue
		}

		// Add the $JIRI_ROOT variable to the run hook environement.
		env := map[string]string{
			"JIRI_ROOT": jirix.Root,
		}

		s := jirix.NewSeq()
		s.Env(env).Verbose(true).Output([]string{fmt.Sprintf("running hook for project %q", op.Project().Name)})
		if err := s.Dir(op.Project().Path).Capture(os.Stdout, os.Stderr).Last(op.Project().RunHook, op.Kind()); err != nil {
			// TODO(nlacasse): Should we delete projectDir or perform some
			// other cleanup in the event of a hook failure?
			return fmt.Errorf("error running hook for project %q: %v", op.Project().Name, err)
		}
	}
	return nil
}

func applyGitHooks(jirix *jiri.X, ops []operation) error {
	jirix.TimerPush("apply githooks")
	defer jirix.TimerPop()
	s := jirix.NewSeq()
	for _, op := range ops {
		if op.Kind() == "create" || op.Kind() == "move" {
			// Apply exclusion for /.jiri/. Ideally we'd only write this file on
			// create, but the remote manifest import is move from the temp directory
			// into the final spot, so we need this to apply to both.
			//
			// TODO(toddw): Find a better way to do this.
			excludeDir := filepath.Join(op.Project().Path, ".git", "info")
			excludeFile := filepath.Join(excludeDir, "exclude")
			excludeString := "/.jiri/\n"
			if err := s.MkdirAll(excludeDir, 0755).WriteFile(excludeFile, []byte(excludeString), 0644).Done(); err != nil {
				return err
			}
		}
		if op.Project().GitHooks == "" {
			continue
		}
		if op.Kind() != "create" && op.Kind() != "move" && op.Kind() != "update" {
			continue
		}
		// Apply git hooks, overwriting any existing hooks.  Jiri is in control of
		// writing all hooks.
		gitHooksDstDir := filepath.Join(op.Project().Path, ".git", "hooks")
		// Copy the specified GitHooks directory into the project's git
		// hook directory.  We walk the file system, creating directories
		// and copying files as we encounter them.
		copyFn := func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			relPath, err := filepath.Rel(op.Project().GitHooks, path)
			if err != nil {
				return err
			}
			dst := filepath.Join(gitHooksDstDir, relPath)
			if info.IsDir() {
				return s.MkdirAll(dst, 0755).Done()
			}
			src, err := s.ReadFile(path)
			if err != nil {
				return err
			}
			// The file *must* be executable to be picked up by git.
			return s.WriteFile(dst, src, 0755).Done()
		}
		if err := filepath.Walk(op.Project().GitHooks, copyFn); err != nil {
			return err
		}
	}
	return nil
}

// writeMetadata stores the given project metadata in the directory
// identified by the given path.
func writeMetadata(jirix *jiri.X, project Project, dir string) (e error) {
	metadataDir := filepath.Join(dir, jiri.ProjectMetaDir)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	defer collect.Error(func() error { return jirix.NewSeq().Chdir(cwd).Done() }, &e)

	s := jirix.NewSeq()
	if err := s.MkdirAll(metadataDir, os.FileMode(0755)).
		Chdir(metadataDir).Done(); err != nil {
		return err
	}
	metadataFile := filepath.Join(metadataDir, jiri.ProjectMetaFile)
	return project.ToFile(jirix, metadataFile)
}

// fsUpdates is used to track filesystem updates made by operations.
// TODO(nlacasse): Currently we only use fsUpdates to track deletions so that
// jiri can delete and create a project in the same directory in one update.
// There are lots of other cases that should be covered though, like detecting
// when two projects would be created in the same directory.
type fsUpdates struct {
	deletedDirs map[string]bool
}

func newFsUpdates() *fsUpdates {
	return &fsUpdates{
		deletedDirs: map[string]bool{},
	}
}

func (u *fsUpdates) deleteDir(dir string) {
	dir = filepath.Clean(dir)
	u.deletedDirs[dir] = true
}

func (u *fsUpdates) isDeleted(dir string) bool {
	_, ok := u.deletedDirs[filepath.Clean(dir)]
	return ok
}

type operation interface {
	// Project identifies the project this operation pertains to.
	Project() Project
	// Kind returns the kind of operation.
	Kind() string
	// Run executes the operation.
	Run(jirix *jiri.X) error
	// String returns a string representation of the operation.
	String() string
	// Test checks whether the operation would fail.
	Test(jirix *jiri.X, updates *fsUpdates) error
}

// commonOperation represents a project operation.
type commonOperation struct {
	// project holds information about the project such as its
	// name, local path, and the protocol it uses for version
	// control.
	project Project
	// destination is the new project path.
	destination string
	// source is the current project path.
	source string
}

func (op commonOperation) Project() Project {
	return op.project
}

// createOperation represents the creation of a project.
type createOperation struct {
	commonOperation
}

func (op createOperation) Kind() string {
	return "create"
}

func (op createOperation) Run(jirix *jiri.X) (e error) {
	s := jirix.NewSeq()

	path, perm := filepath.Dir(op.destination), os.FileMode(0755)
	tmpDirPrefix := strings.Replace(op.Project().Name, "/", ".", -1) + "-"

	// Create a temporary directory for the initial setup of the
	// project to prevent an untimely termination from leaving the
	// $JIRI_ROOT directory in an inconsistent state.
	tmpDir, err := s.MkdirAll(path, perm).TempDir(path, tmpDirPrefix)
	if err != nil {
		return err
	}
	defer collect.Error(func() error { return jirix.NewSeq().RemoveAll(tmpDir).Done() }, &e)
	if err := gitutil.New(jirix.NewSeq()).Clone(op.project.Remote, tmpDir); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	defer collect.Error(func() error { return jirix.NewSeq().Chdir(cwd).Done() }, &e)
	if err := s.Chdir(tmpDir).Done(); err != nil {
		return err
	}
	if err := writeMetadata(jirix, op.project, tmpDir); err != nil {
		return err
	}
	if err := s.Chmod(tmpDir, os.FileMode(0755)).
		Rename(tmpDir, op.destination).Done(); err != nil {
		return err
	}
	return syncProjectMaster(jirix, op.project)
}

func (op createOperation) String() string {
	return fmt.Sprintf("create project %q in %q and advance it to %q", op.project.Name, op.destination, fmtRevision(op.project.Revision))
}

func (op createOperation) Test(jirix *jiri.X, updates *fsUpdates) error {
	// Check the local file system.
	if _, err := jirix.NewSeq().Stat(op.destination); err != nil {
		if !runutil.IsNotExist(err) {
			return err
		}
	} else if !updates.isDeleted(op.destination) {
		return fmt.Errorf("cannot create %q as it already exists", op.destination)
	}
	return nil
}

// deleteOperation represents the deletion of a project.
type deleteOperation struct {
	commonOperation
	// gc determines whether the operation should be executed or
	// whether it should only print a notification.
	gc bool
}

func (op deleteOperation) Kind() string {
	return "delete"
}
func (op deleteOperation) Run(jirix *jiri.X) error {
	s := jirix.NewSeq()
	if op.gc {
		// Never delete projects with non-master branches, uncommitted
		// work, or untracked content.
		git := gitutil.New(jirix.NewSeq(), gitutil.RootDirOpt(op.project.Path))
		branches, _, err := git.GetBranches()
		if err != nil {
			return err
		}
		uncommitted, err := git.HasUncommittedChanges()
		if err != nil {
			return err
		}
		untracked, err := git.HasUntrackedFiles()
		if err != nil {
			return err
		}
		if len(branches) != 1 || uncommitted || untracked {
			lines := []string{
				fmt.Sprintf("NOTE: project %v was not found in the project manifest", op.project.Name),
				"however this project either contains non-master branches, uncommitted",
				"work, or untracked files and will thus not be deleted",
			}
			s.Verbose(true).Output(lines)
			return nil
		}
		return s.RemoveAll(op.source).Done()
	}
	lines := []string{
		fmt.Sprintf("NOTE: project %v was not found in the project manifest", op.project.Name),
		"it was not automatically removed to avoid deleting uncommitted work",
		fmt.Sprintf(`if you no longer need it, invoke "rm -rf %v"`, op.source),
		`or invoke "jiri update -gc" to remove all such local projects`,
	}
	s.Verbose(true).Output(lines)
	return nil
}

func (op deleteOperation) String() string {
	return fmt.Sprintf("delete project %q from %q", op.project.Name, op.source)
}

func (op deleteOperation) Test(jirix *jiri.X, updates *fsUpdates) error {
	if _, err := jirix.NewSeq().Stat(op.source); err != nil {
		if runutil.IsNotExist(err) {
			return fmt.Errorf("cannot delete %q as it does not exist", op.source)
		}
		return err
	}
	updates.deleteDir(op.source)
	return nil
}

// moveOperation represents the relocation of a project.
type moveOperation struct {
	commonOperation
}

func (op moveOperation) Kind() string {
	return "move"
}
func (op moveOperation) Run(jirix *jiri.X) error {
	s := jirix.NewSeq()
	path, perm := filepath.Dir(op.destination), os.FileMode(0755)
	if err := s.MkdirAll(path, perm).Rename(op.source, op.destination).Done(); err != nil {
		return err
	}
	if err := reportNonMaster(jirix, op.project); err != nil {
		return err
	}
	if err := syncProjectMaster(jirix, op.project); err != nil {
		return err
	}
	return writeMetadata(jirix, op.project, op.project.Path)
}

func (op moveOperation) String() string {
	return fmt.Sprintf("move project %q located in %q to %q and advance it to %q", op.project.Name, op.source, op.destination, fmtRevision(op.project.Revision))
}

func (op moveOperation) Test(jirix *jiri.X, updates *fsUpdates) error {
	s := jirix.NewSeq()
	if _, err := s.Stat(op.source); err != nil {
		if runutil.IsNotExist(err) {
			return fmt.Errorf("cannot move %q to %q as the source does not exist", op.source, op.destination)
		}
		return err
	}
	if _, err := s.Stat(op.destination); err != nil {
		if !runutil.IsNotExist(err) {
			return err
		}
	} else {
		return fmt.Errorf("cannot move %q to %q as the destination already exists", op.source, op.destination)
	}
	updates.deleteDir(op.source)
	return nil
}

// updateOperation represents the update of a project.
type updateOperation struct {
	commonOperation
}

func (op updateOperation) Kind() string {
	return "update"
}
func (op updateOperation) Run(jirix *jiri.X) error {
	if err := reportNonMaster(jirix, op.project); err != nil {
		return err
	}
	if err := syncProjectMaster(jirix, op.project); err != nil {
		return err
	}
	return writeMetadata(jirix, op.project, op.project.Path)
}

func (op updateOperation) String() string {
	return fmt.Sprintf("advance project %q located in %q to %q", op.project.Name, op.source, fmtRevision(op.project.Revision))
}

func (op updateOperation) Test(jirix *jiri.X, _ *fsUpdates) error {
	return nil
}

// nullOperation represents a noop.  It is used for logging and adding project
// information to the current manifest.
type nullOperation struct {
	commonOperation
}

func (op nullOperation) Kind() string {
	return "null"
}

func (op nullOperation) Run(jirix *jiri.X) error {
	return writeMetadata(jirix, op.project, op.project.Path)
}

func (op nullOperation) String() string {
	return fmt.Sprintf("project %q located in %q at revision %q is up-to-date", op.project.Name, op.source, fmtRevision(op.project.Revision))
}

func (op nullOperation) Test(jirix *jiri.X, _ *fsUpdates) error {
	return nil
}

// operations is a sortable collection of operations
type operations []operation

// Len returns the length of the collection.
func (ops operations) Len() int {
	return len(ops)
}

// Less defines the order of operations. Operations are ordered first
// by their type and then by their project path.
//
// The order in which operation types are defined determines the order
// in which operations are performed. For correctness and also to
// minimize the chance of a conflict, the delete operations should
// happen before move operations, which should happen before create
// operations. If two create operations make nested directories, the
// outermost should be created first.
func (ops operations) Less(i, j int) bool {
	vals := make([]int, 2)
	for idx, op := range []operation{ops[i], ops[j]} {
		switch op.Kind() {
		case "delete":
			vals[idx] = 0
		case "move":
			vals[idx] = 1
		case "create":
			vals[idx] = 2
		case "update":
			vals[idx] = 3
		case "null":
			vals[idx] = 4
		}
	}
	if vals[0] != vals[1] {
		return vals[0] < vals[1]
	}
	return ops[i].Project().Path < ops[j].Project().Path
}

// Swap swaps two elements of the collection.
func (ops operations) Swap(i, j int) {
	ops[i], ops[j] = ops[j], ops[i]
}

// computeOperations inputs a set of projects to update and the set of
// current and new projects (as defined by contents of the local file
// system and manifest file respectively) and outputs a collection of
// operations that describe the actions needed to update the target
// projects.
func computeOperations(localProjects, remoteProjects Projects, gc bool) operations {
	result := operations{}
	allProjects := map[ProjectKey]bool{}
	for _, p := range localProjects {
		allProjects[p.Key()] = true
	}
	for _, p := range remoteProjects {
		allProjects[p.Key()] = true
	}
	for key, _ := range allProjects {
		var local, remote *Project
		if project, ok := localProjects[key]; ok {
			local = &project
		}
		if project, ok := remoteProjects[key]; ok {
			remote = &project
		}
		result = append(result, computeOp(local, remote, gc))
	}
	sort.Sort(result)
	return result
}

func computeOp(local, remote *Project, gc bool) operation {
	switch {
	case local == nil && remote != nil:
		return createOperation{commonOperation{
			destination: remote.Path,
			project:     *remote,
			source:      "",
		}}
	case local != nil && remote == nil:
		return deleteOperation{commonOperation{
			destination: "",
			project:     *local,
			source:      local.Path,
		}, gc}
	case local != nil && remote != nil:
		switch {
		case local.Path != remote.Path:
			// moveOperation also does an update, so we don't need to check the
			// revision here.
			return moveOperation{commonOperation{
				destination: remote.Path,
				project:     *remote,
				source:      local.Path,
			}}
		case local.Revision != remote.Revision:
			return updateOperation{commonOperation{
				destination: remote.Path,
				project:     *remote,
				source:      local.Path,
			}}
		default:
			return nullOperation{commonOperation{
				destination: remote.Path,
				project:     *remote,
				source:      local.Path,
			}}
		}
	default:
		panic("jiri: computeOp called with nil local and remote")
	}
}

// fmtRevision returns the first 8 chars of a revision hash.
func fmtRevision(r string) string {
	l := 8
	if len(r) < l {
		return r
	}
	return r[:l]
}
