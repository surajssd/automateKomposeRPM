package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os/exec"
	"path"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

// Import struct
type Import struct {
	Name        string
	Version     string
	Subpackages []string
}

// Glide struct
type Glide struct {
	Hash        string
	Updated     string
	Imports     []Import
	TestImports []Import
}

func main() {

	var commit, version string
	flag.StringVar(&commit, "commit", "", "Commit at which release was done")
	flag.StringVar(&version, "version", "", "Version of the current release")
	flag.Parse()

	log.Println("Creating spec file")
	createSpec := fmt.Sprintf("/home/hummer/git/gofed/hack/gofed.sh repo2spec --detect github.com/kubernetes-incubator/kompose --commit %s --with-extra --with-build -f", commit)
	_, err := runCmd(createSpec)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Editing it")
	globals := `%if 0%{?fedora} || 0%{?rhel} == 6
# Not all devel deps exist in Fedora so you can't
# install the devel rpm so we need to build without
# devel or unit_test for now
# Generate devel rpm
%global with_devel 0
# Build project from bundled dependencies
%global with_bundled 1
# Build with debug info rpm
%global with_debug 1
# Run tests in check section
%global with_check 1
# Generate unit-test rpm
%global with_unit_test 0
%else
%global with_devel 0
%global with_bundled 1
%global with_debug 0
%global with_check 0
%global with_unit_test 0
%endif

# https://fedoraproject.org/wiki/PackagingDrafts/Go#Debuginfo
# https://bugzilla.redhat.com/show_bug.cgi?id=995136#c12
%if 0%{?with_debug}
%global _dwz_low_mem_die_limit 0
%else
%global debug_package   %{nil}
%endif

# https://fedoraproject.org/wiki/PackagingDrafts/Go#Debuginfo
%if ! 0%{?gobuild:1}
%define gobuild(o:) go build -ldflags "${LDFLAGS:-} -B 0x$(head -c20 /dev/urandom|od -An -tx1|tr -d ' \\n')" -a -v -x %{?**};
%endif

%global provider        github
%global provider_tld    com
%global project         kubernetes-incubator
%global repo            kompose
# https://github.com/kubernetes-incubator/kompose
%global provider_prefix %{provider}.%{provider_tld}/%{project}/%{repo}
%global import_path     %{provider_prefix}
%global commit          ` + commit + `
%global shortcommit     %(c=%{commit}; echo ${c:0:7})

# define ldflags, buildflags, testflags here. The ldflags/buildflags
# were taken from script/.build and the testflags were taken from
# script/test-unit. We will need to periodically check these for
# consistency.
%global ldflags "-w -X github.com/kubernetes-incubator/kompose/version.GITCOMMIT=%{shortcommit}"
%global buildflags %nil
%global testflags -race -cover -v

Name:           kompose
Version:        ` + version + `
Release:        0.1%{?dist}
Summary:        Tool to move from 'docker-compose' to Kubernetes
License:        ASL 2.0
URL:            https://%{provider_prefix}
Source0:        https://%{provider_prefix}/archive/%{commit}/%{repo}-%{shortcommit}.tar.gz

# e.g. el6 has ppc64 arch without gcc-go, so EA tag is required
ExclusiveArch:  %{?go_arches:%{go_arches}}%{!?go_arches:%{ix86} x86_64 aarch64 %{arm}}
# If go_compiler is not set to 1, there is no virtual provide. Use golang instead.
BuildRequires:  %{?go_compiler:compiler(go-compiler)}%{!?go_compiler:golang}

# Adding dependecy as 'git'
Requires: git

# Main package BuildRequires`
	//stopString := "%global provider        github"
	stopString := "%if ! 0%{?with_bundled}"

	data, err := ioutil.ReadFile("golang-github-kubernetes-incubator-kompose/golang-github-kubernetes-incubator-kompose.spec")
	if err != nil {
		log.Fatalln(err)
	}
	lines := strings.Split(string(data), "\n")

	for i, line := range lines {
		if line == stopString {
			lines = lines[i:]
			break
		}
	}
	lines = append(strings.Split(globals, "\n"), lines...)

	// download glide.lock file and parse it
	url := "https://raw.githubusercontent.com/kubernetes-incubator/kompose/" + commit + "/glide.lock"
	data, err = downloadFile(url)
	if err != nil {
		log.Fatalln(err)
	}

	withBundled, err := parseGlideDeps(data)
	if err != nil {
		log.Fatalln(err)
	}

	stopString = `%description`
	for i, line := range lines {
		if line == stopString {
			startBlock := append(lines[:i], withBundled...)
			startBlock = append(startBlock, "")
			lines = append(startBlock, lines[i:]...)
		}
	}

	mapping := []struct {
		with string
		what []string
	}{
		{
			`%if 0%{?with_check} && ! 0%{?with_bundled}`,
			[]string{`# devel subpackage BuildRequires`, `%if 0%{?with_check} && ! 0%{?with_bundled}`, `# These buildrequires are only for our tests (check)`},
		},
		{
			`%build`,
			[]string{`%build`, `# set up temporary build gopath in pwd`},
		},
		{
			`%check`,
			[]string{`# check uses buildroot macro so that unit-tests can be run over the`, `# files that are about to be installed with the rpm.`, `%check`},
		},
		{
			`#%gobuild -o bin/ %{import_path}/`,
			[]string{`export LDFLAGS=%{ldflags}`, `%gobuild %{buildflags} -o bin/kompose %{import_path}/`},
		},
		{
			`#install -p -m 0755 bin/ %{buildroot}%{_bindir}`,
			[]string{`install -p -m 0755 bin/kompose %{buildroot}%{_bindir}`},
		},
		{
			`%global gotest go test`,
			[]string{`%global gotest go test -ldflags "${LDFLAGS:-}"`},
		},

		{
			`%gotest %{import_path}/pkg/loader/bundle`,
			[]string{`export LDFLAGS=%{ldflags}`,
				`%gotest %{buildflags} %{testflags} %{import_path}/pkg/loader/bundle`},
		},

		{
			`%gotest %{import_path}/pkg/loader/compose`,
			[]string{`%gotest %{buildflags} %{testflags} %{import_path}/pkg/loader/compose`},
		},
		{
			`%gotest %{import_path}/pkg/transformer`,
			[]string{`%gotest %{buildflags} %{testflags} %{import_path}/pkg/transformer`},
		},
		{
			`%gotest %{import_path}/pkg/transformer/kubernetes`,
			[]string{`%gotest %{buildflags} %{testflags} %{import_path}/pkg/transformer/kubernetes`},
		},
		{
			`%gotest %{import_path}/pkg/transformer/openshift`,
			[]string{`%gotest %{buildflags} %{testflags} %{import_path}/pkg/transformer/openshift`},
		},
		{
			`#%{_bindir}/`,
			[]string{`%{_bindir}/kompose`},
		},
	}

	for _, d := range mapping {
		lines = replace(d.with, d.what, lines)
	}

	fmt.Println(strings.Join(lines, "\n"))
}

func runCmd(cmd string) (string, error) {
	command := strings.Split(cmd, " ")
	cmdName := command[0]
	cmdArgs := command[1:]

	var cmdOut []byte
	var err error

	if cmdOut, err = exec.Command(cmdName, cmdArgs...).Output(); err != nil {
		return "", err
	}
	return string(cmdOut), nil
}

func replace(what string, with []string, in []string) []string {

	var loc int
	for i, s := range in {
		if s == what {
			loc = i
			break
		}
	}

	return append(in[:loc], append(with, in[loc+1:]...)...)
}

func downloadFile(url string) ([]byte, error) {
	// Get the data,
	resp, err := http.Get(url)
	if err != nil {
		return []byte{}, err
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return []byte{}, err
	}

	return data, nil
}

func parseGlideDeps(data []byte) ([]string, error) {
	var glide Glide
	err := yaml.Unmarshal(data, &glide)
	if err != nil {
		return []string{}, err
	}

	withBundled := []string{"# Main package Provides", "%if 0%{?with_bundled}"}

	for _, imp := range glide.Imports {
		// we need format like this:
		// Provides: bundled(golang(github.com/coreos/go-oidc/oauth2)) = %{version}-5cf2aa52da8c574d3aa4458f471ad6ae2240fe6b
		for _, subp := range imp.Subpackages {
			name := path.Join(imp.Name, subp)
			withBundled = append(withBundled, fmt.Sprintf("Provides: bundled(golang(%s)) = %s-%s", name, "%{version}", imp.Version))
		}
	}

	return append(withBundled, `%endif`), nil
}
