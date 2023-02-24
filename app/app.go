package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/go-github/v43/github"
)

const (
	inProgress      = "in_progress"
	buildifierCheck = "buildifier"
	nogoCheck       = "bazel"
)

var (
	checks           = []string{"buildifier", "bazel"}
	lineCommentRegex = regexp.MustCompile(`^(?P<file>.*):(?P<line>\d+):(?P<col>\d+):(?P<comment>.*)`)
	urlRegex         = regexp.MustCompile(`Streaming build results to: (?P<url>.*)`)
)

func GetCheckFn(checkName string) (checkFn, error) {
	switch checkName {
	case "buildifier":
		return checkBuildifier, nil
	case "bazel":
		return checkBazelBuild, nil
	}

	return nil, fmt.Errorf("checkFn not found for %q", checkName)
}

type GithubApp struct {
	appID         int64
	appsTransport *ghinstallation.AppsTransport
	transport     *ghinstallation.Transport
	webhookSecret string
	bbAPIKey      string
}

func NewGithubApp(appID int64, privateKeyPath string, webhookSecret string, bbAPIKey string) (*GithubApp, error) {
	appsTransport, err := ghinstallation.NewAppsTransportKeyFromFile(http.DefaultTransport, appID, privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("error creating github app client: %s", err)
	}

	app := &GithubApp{
		appID:         appID,
		webhookSecret: webhookSecret,
		appsTransport: appsTransport,
		bbAPIKey:      bbAPIKey,
	}
	return app, nil
}

func (app *GithubApp) GetClient(installationID int64) *github.Client {
	transport := ghinstallation.NewFromAppsTransport(app.appsTransport, installationID)
	return github.NewClient(&http.Client{Transport: transport})
}

func (app *GithubApp) GetAppClient() *github.Client {
	return github.NewClient(&http.Client{Transport: app.appsTransport})
}

func (app *GithubApp) Token(ctx context.Context, installationID int64) (string, error) {
	tok, res, err := app.GetAppClient().Apps.CreateInstallationToken(ctx, installationID, &github.InstallationTokenOptions{})
	if err := extractError(ctx, res, err); err != nil {
		return "", err
	}
	return tok.GetToken(), nil
}

func extractError(ctx context.Context, res *github.Response, err error) error {
	if err != nil {
		return err
	}
	// If there's an HTTP status >= 400 but the go-github library didn't return an
	// error for whatever reason, manually construct an error.
	if res != nil && res.StatusCode >= 400 {
		return &github.ErrorResponse{
			Response: res.Response,
			Message:  readBody(ctx, res),
		}
	}
	return nil
}

func readBody(ctx context.Context, res *github.Response) string {
	defer res.Body.Close()
	go func() {
		<-ctx.Done()
		res.Body.Close()
	}()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return "<Failed to read body>"
	}
	return string(b)
}

func (app *GithubApp) HandleWebhook(w http.ResponseWriter, req *http.Request) {
	payload, err := github.ValidatePayload(req, []byte(app.webhookSecret))
	if err != nil {
		writeError(w, err)
		return
	}
	event, err := github.ParseWebHook(github.WebHookType(req), payload)
	if err != nil {
		writeError(w, err)
		return
	}

	log.Printf("Got webhook payload of type %T", event)
	ctx := context.Background()

	switch e := event.(type) {
	case *github.CheckSuiteEvent:
		checkSuiteRequested := (e.GetAction() == "requested" || e.GetAction() == "rerequested")
		if checkSuiteRequested {
			err = app.CreateCheckRuns(ctx, e.Installation.GetID(), e.GetRepo(), e.CheckSuite.GetHeadSHA())
		}
	case *github.CheckRunEvent:
		if e.CheckRun.GetApp().GetID() == app.appID {
			switch e.GetAction() {
			case "created":
				err = app.InitCheckRun(ctx, e)
			case "rerequested":
				err = app.CreateCheckRuns(ctx, e.Installation.GetID(), e.GetRepo(), e.CheckRun.GetHeadSHA())
			}
		}
	}
	if err != nil {
		log.Printf("error handling event: %s", err)
	}
}

func (app *GithubApp) InitCheckRun(ctx context.Context, event *github.CheckRunEvent) error {
	owner := event.Repo.GetOwner().GetLogin()
	repo := event.Repo.GetName()
	id := event.CheckRun.GetID()
	installationID := event.Installation.GetID()
	checkName := event.CheckRun.GetName()

	opts := github.UpdateCheckRunOptions{
		Name:   checkName,
		Status: github.String("in_progress"),
	}
	ghc := app.GetClient(installationID)
	updateRun, res, err := ghc.Checks.UpdateCheckRun(ctx, owner, repo, id, opts)
	if err := extractError(ctx, res, err); err != nil {
		return err
	}
	log.Printf("updated Run %v", updateRun)

	fullRepoName := event.Repo.GetFullName()
	headSHA := event.CheckRun.GetHeadSHA()

	// Run a test
	dir := getTmpDir(fullRepoName, checkName)

	err = app.cloneRepo(ctx, fullRepoName, installationID, headSHA, dir)
	if err != nil {
		return fmt.Errorf("failed to clone repo: %s", err)
	}

	checker, err := GetCheckFn(checkName)
	if err != nil {
		return err
	}
	result, err := checker(app, dir)
	if err != nil {
		return fmt.Errorf("failed to run %s: %s", checkName, err)
	}
	if result == nil {
		return fmt.Errorf("failed to run %s", checkName)
	}
	opts = createCompletedUpdateCheckRunOptions(result, checkName)
	updateRun, res, err = ghc.Checks.UpdateCheckRun(ctx, owner, repo, id, opts)
	if err := extractError(ctx, res, err); err != nil {
		return err
	}
	log.Printf("updated Run %v", updateRun)

	err = os.RemoveAll(dir)
	if err != nil {
		log.Printf("failed to cleanup dir %q: %s", dir, err)
	}
	return nil
}

func createCompletedUpdateCheckRunOptions(result *Result, checkName string) github.UpdateCheckRunOptions {
	output := &github.CheckRunOutput{
		Title:   github.String(result.Title),
		Summary: github.String(result.Summary),
	}

	if len(result.Annotations) > 0 {
		output.Annotations = []*github.CheckRunAnnotation{}
	}
	for _, a := range result.Annotations {
		output.Annotations = append(output.Annotations, &github.CheckRunAnnotation{
			Path:            github.String(a.Path),
			StartLine:       github.Int(a.Line),
			EndLine:         github.Int(a.Line),
			AnnotationLevel: github.String(a.Severity),
			Message:         github.String(a.Message),
		})
	}
	opts := github.UpdateCheckRunOptions{
		Name:       checkName,
		Status:     github.String("completed"),
		Conclusion: github.String(result.Conclusion),
		Output:     output,
	}
	if result.URL != "" {
		opts.DetailsURL = github.String(result.URL)
	}
	return opts
}

func getTmpDir(fullRepoName string, checkName string) string {
	return fmt.Sprintf("/tmp/%s/%s", fullRepoName, checkName)
}

type checkFn func(app *GithubApp, dir string) (*Result, error)

func (app *GithubApp) CreateCheckRuns(ctx context.Context, installationID int64, repo *github.Repository, headSHA string) error {
	owner := repo.GetOwner().GetLogin()
	repoName := repo.GetName()

	for _, checkName := range checks {
		opts := github.CreateCheckRunOptions{
			Name:    checkName,
			HeadSHA: headSHA,
		}
		_, res, err := app.GetClient(installationID).Checks.CreateCheckRun(ctx, owner, repoName, opts)
		if err := extractError(ctx, res, err); err != nil {
			return err
		}
		log.Printf("checkRun created: %s", checkName)
	}
	return nil
}

func writeError(w http.ResponseWriter, err error) {
	statusCode := 500
	if err, ok := err.(*github.ErrorResponse); ok && err.Response != nil {
		statusCode = err.Response.StatusCode
	}
	http.Error(w, err.Error(), statusCode)
}

func (app *GithubApp) cloneRepo(ctx context.Context, fullRepoName string, installationID int64, headSHA string, targetDir string) error {
	token, err := app.Token(ctx, installationID)
	if err != nil {
		return fmt.Errorf("failed to get token: %s", err)
	}
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, fullRepoName)
	r, err := git.PlainCloneContext(ctx, targetDir, false, &git.CloneOptions{
		URL:      url,
		Progress: os.Stdout,
	})
	if err != nil {
		return fmt.Errorf("unable to clone repo to %q: %s", targetDir, err)
	}

	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get work tree: %s", err)
	}

	err = w.Pull(&git.PullOptions{})

	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("failed to pull: %s", err)
	}

	err = w.Checkout(&git.CheckoutOptions{
		Hash:  plumbing.NewHash(headSHA),
		Force: true,
	})
	if err != nil {
		return fmt.Errorf("failed to checkout %q: %s", headSHA, err)
	}

	return nil
}

func runCmd(toolName string, arg ...string) (bytes.Buffer, bytes.Buffer, error) {
	var output, stderr bytes.Buffer
	cmd := exec.Command(toolName, arg...)
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err != nil {
		log.Printf("check failed for cmd %q: %v", cmd, err)
	}
	if stderr.Len() > 0 {
		log.Printf("output: %s, %s", output.String(), stderr.String())
		return output, stderr, nil
	}
	return output, stderr, err
}

type Result struct {
	Title       string
	Summary     string
	Conclusion  string
	Annotations []*Annotation
	URL         string
}

type Annotation struct {
	Message  string
	Line     int
	Path     string
	Severity string
}

// checkBuildifier checks if the given file is formatted according to buildifier and, if not, prints
// a diff detailing what's wrong with the file to stdout and returns an error.
func checkBuildifier(_ *GithubApp, dir string) (*Result, error) {
	_, stdErr, err := runCmd("buildifier", "--mode=check", "-r", dir)
	if stdErr.Len() == 0 {
		return nil, err
	}

	scanner := bufio.NewScanner(&stdErr)
	annotations := []*Annotation{}
	res := &Result{
		Title: "Buildifier Lint Result",
	}

	for scanner.Scan() {
		line := scanner.Text()
		log.Printf("scanner: %q", line)
		parts := strings.Split(line, "#")
		if len(parts) > 0 {
			rel, err := filepath.Rel(dir, strings.TrimSpace(parts[0]))
			if err != nil {
				log.Printf("failed to get reletive path: %s", err)
			}
			annotations = append(annotations, &Annotation{
				Message:  fmt.Sprintf("file %q needs reformat", rel),
				Severity: "failure",
				Path:     rel,
				Line:     1,
			})
		}
	}

	if len(annotations) == 0 {
		res.Summary = "No issues found."
		res.Conclusion = "success"
	} else {
		res.Summary = fmt.Sprintf("%d BUILD files need reformat", len(annotations))
		res.Conclusion = "failure"
		res.Annotations = annotations
	}
	return res, nil
}

func checkBazelBuild(app *GithubApp, dir string) (*Result, error) {
	curDir, err := os.Getwd()
	if err != nil {
		return nil, errors.New("failed to get current directory")
	}
	err = os.Chdir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to change directory to %q: %s", dir, err)
	}

	stdOut, _, err := runCmd("bb", "build", "//...", fmt.Sprintf("--remote_header=x-buildbuddy-api-key=%s", app.bbAPIKey))
	if stdOut.Len() == 0 {
		return nil, err
	}
	scanner := bufio.NewScanner(&stdOut)

	res := &Result{
		Title: "Build result",
	}
	annotations := []*Annotation{}

	url := ""
	// dedupe
	m := make(map[string]struct{})

	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)

		// check url
		if url == "" {
			urlIndex := urlRegex.SubexpIndex("url")
			matches := urlRegex.FindStringSubmatch(line)
			if len(matches) > 0 {
				url = matches[urlIndex]
				log.Printf("find url: %q", url)
			}
		}

		// check errors
		if strings.HasPrefix(line, "ERROR: ") || strings.HasPrefix(line, "INFO: ") || strings.HasPrefix(line, "FAILED: ") {
			continue
		}
		fileIndex := lineCommentRegex.SubexpIndex("file")
		lineIndex := lineCommentRegex.SubexpIndex("line")
		commentIndex := lineCommentRegex.SubexpIndex("comment")
		matches := lineCommentRegex.FindStringSubmatch(line)
		if len(matches) > 0 {
			if _, ok := m[line]; ok {
				continue
			}
			file := matches[fileIndex]
			lineNumStr := matches[lineIndex]
			lineNum, err := strconv.Atoi(lineNumStr)
			if err != nil {
				log.Printf("unable to parse string %q to int", lineNumStr)
			}
			comment := matches[commentIndex]
			annotations = append(annotations, &Annotation{
				Message:  comment,
				Severity: "failure",
				Path:     file,
				Line:     lineNum,
			})
			m[line] = struct{}{}
			log.Println(line)
		}
	}
	if len(annotations) == 0 {
		res.Summary = "No issues found."
		res.Conclusion = "success"
	} else {
		res.Summary = "Build doesn't complete successfully"
		res.Conclusion = "failure"
		res.Annotations = annotations
	}
	res.URL = url

	err = os.Chdir(curDir)
	if err != nil {
		return nil, fmt.Errorf("failed to change directory to %q: %s", curDir, err)
	}
	return res, nil
}