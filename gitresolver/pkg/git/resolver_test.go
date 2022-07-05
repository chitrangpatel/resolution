package git

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	"github.com/tektoncd/resolution/pkg/apis/resolution/v1alpha1"
	resolutioncommon "github.com/tektoncd/resolution/pkg/common"
	ttesting "github.com/tektoncd/resolution/pkg/reconciler/testing"
	"github.com/tektoncd/resolution/pkg/resolver/framework"
	frtesting "github.com/tektoncd/resolution/pkg/resolver/framework/testing"
	"github.com/tektoncd/resolution/test"
	"github.com/tektoncd/resolution/test/diff"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/system"

	_ "knative.dev/pkg/system/testing"
)

func TestGetSelector(t *testing.T) {
	resolver := Resolver{}
	sel := resolver.GetSelector(context.Background())
	if typ, has := sel[resolutioncommon.LabelKeyResolverType]; !has {
		t.Fatalf("unexpected selector: %v", sel)
	} else if typ != LabelValueGitResolverType {
		t.Fatalf("unexpected type: %q", typ)
	}
}

func TestValidateParams(t *testing.T) {
	resolver := Resolver{}

	paramsWithCommit := map[string]string{
		PathParam:   "bar",
		CommitParam: "baz",
	}
	if err := resolver.ValidateParams(context.Background(), paramsWithCommit); err != nil {
		t.Fatalf("unexpected error validating params: %v", err)
	}

	paramsWithBranch := map[string]string{
		PathParam:   "bar",
		BranchParam: "baz",
	}
	if err := resolver.ValidateParams(context.Background(), paramsWithBranch); err != nil {
		t.Fatalf("unexpected error validating params: %v", err)
	}
}

func TestValidateParamsMissing(t *testing.T) {
	resolver := Resolver{}

	var err error

	paramsMissingPath := map[string]string{
		URLParam:    "foo",
		BranchParam: "baz",
	}
	err = resolver.ValidateParams(context.Background(), paramsMissingPath)
	if err == nil {
		t.Fatalf("expected missing pathInRepo err")
	}
}

func TestValidateParamsConflictingGitRef(t *testing.T) {
	resolver := Resolver{}
	params := map[string]string{
		URLParam:    "foo",
		PathParam:   "bar",
		CommitParam: "baz",
		BranchParam: "quux",
	}
	err := resolver.ValidateParams(context.Background(), params)
	if err == nil {
		t.Fatalf("expected err due to conflicting commit and branch params")
	}
}

func TestGetResolutionTimeoutDefault(t *testing.T) {
	resolver := Resolver{}
	defaultTimeout := 30 * time.Minute
	timeout := resolver.GetResolutionTimeout(context.Background(), defaultTimeout)
	if timeout != defaultTimeout {
		t.Fatalf("expected default timeout to be returned")
	}
}

func TestGetResolutionTimeoutCustom(t *testing.T) {
	resolver := Resolver{}
	defaultTimeout := 30 * time.Minute
	configTimeout := 5 * time.Second
	config := map[string]string{
		ConfigFieldTimeout: configTimeout.String(),
	}
	ctx := framework.InjectResolverConfigToContext(context.Background(), config)
	timeout := resolver.GetResolutionTimeout(ctx, defaultTimeout)
	if timeout != configTimeout {
		t.Fatalf("expected timeout from config to be returned")
	}
}

func TestResolve(t *testing.T) {
	withTemporaryGitConfig(t)

	testCases := []struct {
		name            string
		commits         []commitForRepo
		branch          string
		useNthCommit    int
		specificCommit  string
		pathInRepo      string
		expectedContent []byte
		expectedErr     error
	}{
		{
			name: "single commit",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			pathInRepo:      "foo/bar/somefile",
			expectedContent: []byte("some content"),
		}, {
			name: "with branch",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
				Branch:   "other-branch",
			}, {
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "wrong content",
			}},
			branch:          "other-branch",
			pathInRepo:      "foo/bar/somefile",
			expectedContent: []byte("some content"),
		}, {
			name: "earlier specific commit",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}, {
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "different content",
			}},
			pathInRepo:      "foo/bar/somefile",
			useNthCommit:    1,
			expectedContent: []byte("different content"),
		}, {
			name: "file does not exist",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			pathInRepo:  "foo/bar/some other file",
			expectedErr: errors.New(`error opening file "foo/bar/some other file": file does not exist`),
		}, {
			name: "branch does not exist",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			branch:      "does-not-exist",
			pathInRepo:  "foo/bar/some other file",
			expectedErr: errors.New(`clone error: couldn't find remote ref "refs/heads/does-not-exist"`),
		}, {
			name: "commit does not exist",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			specificCommit: "does-not-exist",
			pathInRepo:     "foo/bar/some other file",
			expectedErr:    errors.New("checkout error: object not found"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repoPath, commits := createTestRepo(t, tc.commits)
			resolver := &Resolver{}

			params := map[string]string{
				URLParam:  repoPath,
				PathParam: tc.pathInRepo,
			}

			if tc.branch != "" {
				params[BranchParam] = tc.branch
			}

			if tc.useNthCommit > 0 {
				params[CommitParam] = commits[plumbing.Master.Short()][tc.useNthCommit]
			} else if tc.specificCommit != "" {
				params[CommitParam] = hex.EncodeToString([]byte(tc.specificCommit))
			}
			output, err := resolver.Resolve(context.Background(), params)
			if tc.expectedErr != nil {
				if err == nil {
					t.Fatalf("expected err '%v' but didn't get one", tc.expectedErr)
				}
				if tc.expectedErr.Error() != err.Error() {
					t.Fatalf("expected err '%v' but got '%v'", tc.expectedErr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error resolving: %v", err)
				}

				expectedResource := &ResolvedGitResource{
					Content: tc.expectedContent,
				}
				switch {
				case tc.useNthCommit > 0:
					expectedResource.Commit = commits[plumbing.Master.Short()][tc.useNthCommit]
				case tc.branch != "":
					expectedResource.Commit = commits[tc.branch][len(commits[tc.branch])-1]
				default:
					expectedResource.Commit = commits[plumbing.Master.Short()][len(commits[plumbing.Master.Short()])-1]
				}

				if d := cmp.Diff(expectedResource, output); d != "" {
					t.Errorf("unexpected resource from Resolve: %s", diff.PrintWantGot(d))
				}
			}
		})
	}
}

func TestController(t *testing.T) {
	withTemporaryGitConfig(t)

	testCases := []struct {
		name           string
		commits        []commitForRepo
		branch         string
		useNthCommit   int
		specificCommit string
		pathInRepo     string
		expectedStatus *v1alpha1.ResolutionRequestStatus
		expectedErr    error
	}{
		{
			name: "single commit",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			pathInRepo: "foo/bar/somefile",
			expectedStatus: &v1alpha1.ResolutionRequestStatus{
				Status: duckv1.Status{
					Annotations: map[string]string{
						"content-type": "application/x-yaml",
					},
				},
				ResolutionRequestStatusFields: v1alpha1.ResolutionRequestStatusFields{
					Data: base64.StdEncoding.Strict().EncodeToString([]byte("some content")),
				},
			},
		}, {
			name: "with branch",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
				Branch:   "other-branch",
			}, {
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "wrong content",
			}},
			branch:     "other-branch",
			pathInRepo: "foo/bar/somefile",
			expectedStatus: &v1alpha1.ResolutionRequestStatus{
				Status: duckv1.Status{
					Annotations: map[string]string{
						"content-type": "application/x-yaml",
					},
				},
				ResolutionRequestStatusFields: v1alpha1.ResolutionRequestStatusFields{
					Data: base64.StdEncoding.Strict().EncodeToString([]byte("some content")),
				},
			},
		}, {
			name: "earlier specific commit",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}, {
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "different content",
			}},
			pathInRepo:   "foo/bar/somefile",
			useNthCommit: 1,
			expectedStatus: &v1alpha1.ResolutionRequestStatus{
				Status: duckv1.Status{
					Annotations: map[string]string{
						"content-type": "application/x-yaml",
					},
				},
				ResolutionRequestStatusFields: v1alpha1.ResolutionRequestStatusFields{
					Data: base64.StdEncoding.Strict().EncodeToString([]byte("different content")),
				},
			},
		}, {
			name: "file does not exist",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			pathInRepo: "foo/bar/some other file",
			expectedStatus: &v1alpha1.ResolutionRequestStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{{
						Type:   apis.ConditionSucceeded,
						Status: corev1.ConditionFalse,
						Reason: resolutioncommon.ReasonResolutionFailed,
					}},
				},
			},
			expectedErr: errors.New(`error getting "Git" "foo/rr": error opening file "foo/bar/some other file": file does not exist`),
		}, {
			name: "branch does not exist",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			branch:     "does-not-exist",
			pathInRepo: "foo/bar/some other file",
			expectedStatus: &v1alpha1.ResolutionRequestStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{{
						Type:   apis.ConditionSucceeded,
						Status: corev1.ConditionFalse,
						Reason: resolutioncommon.ReasonResolutionFailed,
					}},
				},
			},
			expectedErr: errors.New(`error getting "Git" "foo/rr": clone error: couldn't find remote ref "refs/heads/does-not-exist"`),
		}, {
			name: "commit does not exist",
			commits: []commitForRepo{{
				Dir:      "foo/bar",
				Filename: "somefile",
				Content:  "some content",
			}},
			specificCommit: "does-not-exist",
			pathInRepo:     "foo/bar/some other file",
			expectedStatus: &v1alpha1.ResolutionRequestStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{{
						Type:   apis.ConditionSucceeded,
						Status: corev1.ConditionFalse,
						Reason: resolutioncommon.ReasonResolutionFailed,
					}},
				},
			},
			expectedErr: errors.New(`error getting "Git" "foo/rr": checkout error: object not found`),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := ttesting.SetupFakeContext(t)

			repoPath, commits := createTestRepo(t, tc.commits)

			request := createRequest(repoPath, tc.pathInRepo, tc.branch, tc.specificCommit, tc.useNthCommit, commits)
			resolver := &Resolver{}

			var expectedStatus *v1alpha1.ResolutionRequestStatus
			if tc.expectedStatus != nil {
				expectedStatus = tc.expectedStatus.DeepCopy()

				if tc.expectedErr == nil {
					// Add the expected commit to the expected status annotations, but only if we expect success.
					if cmt, ok := request.Spec.Parameters[CommitParam]; ok {
						expectedStatus.Annotations[AnnotationKeyCommitHash] = cmt
					} else {
						branchForCommit := plumbing.Master.Short()
						if tc.branch != "" {
							branchForCommit = tc.branch
						}
						if _, ok := commits[branchForCommit]; ok {
							cmt := commits[branchForCommit][len(commits[branchForCommit])-1]
							expectedStatus.Annotations[AnnotationKeyCommitHash] = cmt
						}
					}
				} else {
					expectedStatus.Status.Conditions[0].Message = tc.expectedErr.Error()
				}
			}
			d := test.Data{
				ConfigMaps: []*corev1.ConfigMap{{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resolver.GetConfigName(ctx),
						Namespace: system.Namespace(),
					},
					Data: map[string]string{
						ConfigFieldTimeout: "1m",
					},
				}},
				ResolutionRequests: []*v1alpha1.ResolutionRequest{request},
			}

			frtesting.RunResolverReconcileTest(ctx, t, d, resolver, request, expectedStatus, tc.expectedErr)
		})
	}
}

// createTestRepo is used to instantiate a local test repository with the desired commits.
func createTestRepo(t *testing.T, commits []commitForRepo) (string, map[string][]string) {
	t.Helper()
	tempDir := t.TempDir()

	repo, err := git.PlainInit(tempDir, false)

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("getting test worktree: %v", err)
	}
	if worktree == nil {
		t.Fatal("test worktree not created")
	}

	startingHash := writeAndCommitToTestRepo(t, worktree, tempDir, "", "README", []byte("This is a test"))

	hashesByBranch := make(map[string][]string)

	// Iterate over the commits and add them.
	for _, cmt := range commits {
		branch := cmt.Branch
		if branch == "" {
			branch = plumbing.Master.Short()
		}

		// If we're given a branch, check out that branch.
		coOpts := &git.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(branch),
		}

		if _, ok := hashesByBranch[branch]; !ok && branch != plumbing.Master.Short() {
			coOpts.Hash = plumbing.NewHash(startingHash)
			coOpts.Create = true
		}

		if err := worktree.Checkout(coOpts); err != nil {
			t.Fatalf("couldn't do checkout of %s: %v", branch, err)
		}

		hash := writeAndCommitToTestRepo(t, worktree, tempDir, cmt.Dir, cmt.Filename, []byte(cmt.Content))

		if _, ok := hashesByBranch[branch]; !ok {
			hashesByBranch[branch] = []string{hash}
		} else {
			hashesByBranch[branch] = append(hashesByBranch[branch], hash)
		}
	}

	return tempDir, hashesByBranch
}

// commitForRepo provides the directory, filename, content and branch for a test commit.
type commitForRepo struct {
	Dir      string
	Filename string
	Content  string
	Branch   string
}

func writeAndCommitToTestRepo(t *testing.T, worktree *git.Worktree, repoDir string, subPath string, filename string, content []byte) string {
	t.Helper()

	targetDir := repoDir
	if subPath != "" {
		targetDir = filepath.Join(targetDir, subPath)
		fi, err := os.Stat(targetDir)
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetDir, 0700); err != nil {
				t.Fatalf("couldn't create directory %s in worktree: %v", targetDir, err)
			}
		} else if err != nil {
			t.Fatalf("checking if directory %s in worktree exists: %v", targetDir, err)
		}
		if fi != nil && !fi.IsDir() {
			t.Fatalf("%s already exists but is not a directory", targetDir)
		}
	}

	outfile := filepath.Join(targetDir, filename)
	if err := ioutil.WriteFile(outfile, content, 0600); err != nil {
		t.Fatalf("couldn't write content to file %s: %v", outfile, err)
	}

	_, err := worktree.Add(filepath.Join(subPath, filename))
	if err != nil {
		t.Fatalf("couldn't add file %s to git: %v", outfile, err)
	}

	hash, err := worktree.Commit("adding file for test", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Someone",
			Email: "someone@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("couldn't perform commit for test: %v", err)
	}

	return hash.String()
}

// withTemporaryGitConfig resets the .gitconfig for the duration of the test.
func withTemporaryGitConfig(t *testing.T) func() {
	gitConfigDir := t.TempDir()
	key := "GIT_CONFIG_GLOBAL"
	t.Helper()
	oldValue, envVarExists := os.LookupEnv(key)
	if err := os.Setenv(key, filepath.Join(gitConfigDir, "config")); err != nil {
		t.Fatal(err)
	}
	clean := func() {
		t.Helper()
		if !envVarExists {
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := os.Setenv(key, oldValue); err != nil {
			t.Fatal(err)
		}
	}
	return clean
}

func createRequest(repoURL, pathInRepo, branch, specificCommit string, useNthCommit int, commitsByBranch map[string][]string) *v1alpha1.ResolutionRequest {
	rr := &v1alpha1.ResolutionRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "resolution.tekton.dev/v1alpha1",
			Kind:       "ResolutionRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "rr",
			Namespace:         "foo",
			CreationTimestamp: metav1.Time{Time: time.Now()},
			Labels: map[string]string{
				resolutioncommon.LabelKeyResolverType: LabelValueGitResolverType,
			},
		},
		Spec: v1alpha1.ResolutionRequestSpec{
			Parameters: map[string]string{
				URLParam:  repoURL,
				PathParam: pathInRepo,
			},
		},
	}

	if branch != "" {
		rr.Spec.Parameters[BranchParam] = branch
	}

	if useNthCommit > 0 {
		rr.Spec.Parameters[CommitParam] = commitsByBranch[plumbing.Master.Short()][useNthCommit]
	} else if specificCommit != "" {
		rr.Spec.Parameters[CommitParam] = hex.EncodeToString([]byte(specificCommit))
	}

	return rr
}
