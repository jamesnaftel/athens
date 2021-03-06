package module

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gomods/athens/pkg/errors"
	"github.com/gomods/athens/pkg/observ"
	"github.com/gomods/athens/pkg/storage"
	"github.com/spf13/afero"
)

type goGetFetcher struct {
	fs           afero.Fs
	goBinaryName string
	goProxy      string
}

type goModule struct {
	Path     string `json:"path"`     // module path
	Version  string `json:"version"`  // module version
	Error    string `json:"error"`    // error loading module
	Info     string `json:"info"`     // absolute path to cached .info file
	GoMod    string `json:"goMod"`    // absolute path to cached .mod file
	Zip      string `json:"zip"`      // absolute path to cached .zip file
	Dir      string `json:"dir"`      // absolute path to cached source root directory
	Sum      string `json:"sum"`      // checksum for path, version (as in go.sum)
	GoModSum string `json:"goModSum"` // checksum for go.mod (as in go.sum)
}

// NewGoGetFetcher creates fetcher which uses go get tool to fetch modules
func NewGoGetFetcher(goBinaryName string, goProxy string, fs afero.Fs) (Fetcher, error) {
	const op errors.Op = "module.NewGoGetFetcher"
	if err := validGoBinary(goBinaryName); err != nil {
		return nil, errors.E(op, err)
	}
	return &goGetFetcher{
		fs:           fs,
		goBinaryName: goBinaryName,
		goProxy:      goProxy,
	}, nil
}

// Fetch downloads the sources from the go binary and returns the corresponding
// .info, .mod, and .zip files.
func (g *goGetFetcher) Fetch(ctx context.Context, mod, ver string) (*storage.Version, error) {
	const op errors.Op = "goGetFetcher.Fetch"
	ctx, span := observ.StartSpan(ctx, op.String())
	defer span.End()

	// setup the GOPATH
	goPathRoot, err := afero.TempDir(g.fs, "", "athens")
	if err != nil {
		return nil, errors.E(op, err)
	}
	sourcePath := filepath.Join(goPathRoot, "src")
	modPath := filepath.Join(sourcePath, getRepoDirName(mod, ver))
	if err := g.fs.MkdirAll(modPath, os.ModeDir|os.ModePerm); err != nil {
		ClearFiles(g.fs, goPathRoot)
		return nil, errors.E(op, err)
	}

	m, err := downloadModule(g.goBinaryName, g.goProxy, g.fs, goPathRoot, modPath, mod, ver)
	if err != nil {
		ClearFiles(g.fs, goPathRoot)
		return nil, errors.E(op, err)
	}

	var storageVer storage.Version
	storageVer.Semver = m.Version
	info, err := afero.ReadFile(g.fs, m.Info)
	if err != nil {
		return nil, errors.E(op, err)
	}
	storageVer.Info = info

	gomod, err := afero.ReadFile(g.fs, m.GoMod)
	if err != nil {
		return nil, errors.E(op, err)
	}
	storageVer.Mod = gomod

	zip, err := g.fs.Open(m.Zip)
	if err != nil {
		return nil, errors.E(op, err)
	}
	// note: don't close zip here so that the caller can read directly from disk.
	//
	// if we close, then the caller will panic, and the alternative to make this work is
	// that we read into memory and return an io.ReadCloser that reads out of memory
	storageVer.Zip = &zipReadCloser{zip, g.fs, goPathRoot}

	return &storageVer, nil
}

// given a filesystem, gopath, repository root, module and version, runs 'go mod download -json'
// on module@version from the repoRoot with GOPATH=gopath, and returns a non-nil error if anything went wrong.
func downloadModule(goBinaryName, goProxy string, fs afero.Fs, gopath, repoRoot, module, version string) (goModule, error) {
	const op errors.Op = "module.downloadModule"
	uri := strings.TrimSuffix(module, "/")
	fullURI := fmt.Sprintf("%s@%s", uri, version)

	cmd := exec.Command(goBinaryName, "mod", "download", "-json", fullURI)
	cmd.Env = PrepareEnv(gopath, goProxy)
	cmd.Dir = repoRoot
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%v: %s", err, stderr)
		var m goModule
		if jsonErr := json.NewDecoder(stdout).Decode(&m); jsonErr != nil {
			return goModule{}, errors.E(op, err)
		}
		// github quota exceeded
		if isLimitHit(m.Error) {
			return goModule{}, errors.E(op, m.Error, errors.KindRateLimit)
		}
		return goModule{}, errors.E(op, m.Error, errors.KindNotFound)
	}

	var m goModule
	if err = json.NewDecoder(stdout).Decode(&m); err != nil {
		return goModule{}, errors.E(op, err)
	}
	if m.Error != "" {
		return goModule{}, errors.E(op, m.Error)
	}

	return m, nil
}

// PrepareEnv will return all the appropriate
// environment variables for a Go Command to run
// successfully (such as GOPATH, GOCACHE, PATH etc)
func PrepareEnv(gopath, goProxy string) []string {
	pathEnv := fmt.Sprintf("PATH=%s", os.Getenv("PATH"))
	homeEnv := fmt.Sprintf("HOME=%s", os.Getenv("HOME"))
	httpProxy := fmt.Sprintf("HTTP_PROXY=%s", os.Getenv("HTTP_PROXY"))
	httpsProxy := fmt.Sprintf("HTTPS_PROXY=%s", os.Getenv("HTTPS_PROXY"))
	noProxy := fmt.Sprintf("NO_PROXY=%s", os.Getenv("NO_PROXY"))
	// need to also check the lower case version of just these three env variables
	httpProxyLower := fmt.Sprintf("http_proxy=%s", os.Getenv("http_proxy"))
	httpsProxyLower := fmt.Sprintf("https_proxy=%s", os.Getenv("https_proxy"))
	noProxyLower := fmt.Sprintf("no_proxy=%s", os.Getenv("no_proxy"))
	gopathEnv := fmt.Sprintf("GOPATH=%s", gopath)
	goProxyEnv := fmt.Sprintf("GOPROXY=%s", goProxy)
	cacheEnv := fmt.Sprintf("GOCACHE=%s", filepath.Join(gopath, "cache"))
	gitSSH := fmt.Sprintf("GIT_SSH=%s", os.Getenv("GIT_SSH"))
	gitSSHCmd := fmt.Sprintf("GIT_SSH_COMMAND=%s", os.Getenv("GIT_SSH_COMMAND"))
	disableCgo := "CGO_ENABLED=0"
	enableGoModules := "GO111MODULE=on"
	cmdEnv := []string{
		pathEnv,
		homeEnv,
		gopathEnv,
		goProxyEnv,
		cacheEnv,
		disableCgo,
		enableGoModules,
		httpProxy,
		httpsProxy,
		noProxy,
		httpProxyLower,
		httpsProxyLower,
		noProxyLower,
		gitSSH,
		gitSSHCmd,
	}

	if sshAuthSockVal, hasSSHAuthSock := os.LookupEnv("SSH_AUTH_SOCK"); hasSSHAuthSock {
		// Verify that the ssh agent unix socket exists and is a unix socket.
		st, err := os.Stat(sshAuthSockVal)
		if err == nil && st.Mode()&os.ModeSocket != 0 {
			sshAuthSock := fmt.Sprintf("SSH_AUTH_SOCK=%s", sshAuthSockVal)
			cmdEnv = append(cmdEnv, sshAuthSock)
		}
	}

	// add Windows specific ENV VARS
	if runtime.GOOS == "windows" {
		cmdEnv = append(cmdEnv, fmt.Sprintf("USERPROFILE=%s", os.Getenv("USERPROFILE")))
		cmdEnv = append(cmdEnv, fmt.Sprintf("SystemRoot=%s", os.Getenv("SystemRoot")))
		cmdEnv = append(cmdEnv, fmt.Sprintf("ALLUSERSPROFILE=%s", os.Getenv("ALLUSERSPROFILE")))
		cmdEnv = append(cmdEnv, fmt.Sprintf("HOMEDRIVE=%s", os.Getenv("HOMEDRIVE")))
		cmdEnv = append(cmdEnv, fmt.Sprintf("HOMEPATH=%s", os.Getenv("HOMEPATH")))
	}

	return cmdEnv
}

func isLimitHit(o string) bool {
	return strings.Contains(o, "403 response from api.github.com")
}

// getRepoDirName takes a raw repository URI and a version and creates a directory name that the
// repository contents can be put into
func getRepoDirName(repoURI, version string) string {
	escapedURI := strings.Replace(repoURI, "/", "-", -1)
	return fmt.Sprintf("%s-%s", escapedURI, version)
}

func validGoBinary(name string) error {
	const op errors.Op = "module.validGoBinary"
	err := exec.Command(name).Run()
	_, ok := err.(*exec.ExitError)
	if err != nil && !ok {
		return errors.E(op, err)
	}
	return nil
}
