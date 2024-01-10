package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/go-git/go-git/v5/config"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/go-github/v42/github"
	"github.com/pterm/pterm"
	"golang.org/x/oauth2"
)

type cloneTask struct {
	repoURL       string
	destDir       string
	defaultBranch string
}

var wg sync.WaitGroup
var cloneTasksChan = make(chan cloneTask)
var totalTasks, doneTasks int
var timeoutFlag = flag.String("timeout", "2m", "timeout duration for git operations")
var totalBar, _ = pterm.DefaultProgressbar.WithTitle("Cloning Projects").Start() // declare totalBar with an initial value
var defaultBranch = "main"

func main() {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		pterm.Fatal.Println("GITHUB_TOKEN not set")
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	gitHubClient := github.NewClient(tc)

	baseDir := flag.String("path", "./", "Path to clone repositories")
	orgOrUser := flag.String("target", "", "GitHub organization or user to clone from")

	flag.Parse()

	if *orgOrUser == "" {
		pterm.Error.Println("Please specify a target organization or user with the -target option")
		os.Exit(1)
	}

	if _, err := time.ParseDuration(*timeoutFlag); err != nil {
		pterm.Error.Printf("Invalid timeout duration: %s", *timeoutFlag)
		os.Exit(1)
	}

	totalBar, _ := pterm.DefaultProgressbar.WithTitle("Cloning GitHub Repositories").Start()
	go cloneWorker(cloneTasksChan)

	wg.Add(1)
	go cloneAllGitHubRepositories(ctx, gitHubClient, *orgOrUser, *baseDir)

	wg.Wait()
	close(cloneTasksChan)

	totalBar.Stop()

	pterm.Success.Printf("Cloned %d repositories from GitHub\n", doneTasks)
}

func cloneAllGitHubRepositories(ctx context.Context, client *github.Client, target, baseDir string) {
	defer wg.Done()

	// Initialize the options for listing repositories, ensuring we include private ones.
	opt := &github.RepositoryListOptions{
		ListOptions: github.ListOptions{PerPage: 100},
		Type:        "all",
		Visibility:  "all",
	}

	// Loop through all pages of repositories
	for {
		// Get the list of repositories for the current page
		repos, resp, err := client.Repositories.List(ctx, target, opt)
		if err != nil {
			pterm.Error.Printf("Failed to list repositories for %s: %v\n", target, err)
			return
		}

		// Update the total task count and the progress bar's total.
		totalTasks += len(repos)
		totalBar.WithTotal(totalTasks)

		// Queue each repository for cloning
		for _, repo := range repos {
			if repo.Archived != nil && *repo.Archived {
				continue // Skip archived repositories.
			}

			var branchName string
			if repo.GetDefaultBranch() != "" {
				branchName = repo.GetDefaultBranch()
			} else {
				branchName = defaultBranch // Default branch name if not provided by GitHub
			}

			repoName := *repo.Name
			repoURL := repo.GetCloneURL()
			destDir := filepath.Join(baseDir, repoName)

			// Send the clone task to the worker
			wg.Add(1)
			cloneTasksChan <- cloneTask{repoURL: repoURL, destDir: destDir, defaultBranch: branchName}
		}

		// Check if we've reached the last page; if so, break out of the loop
		if resp.NextPage == 0 {
			break
		}
		// Set the page number for the next request
		opt.Page = resp.NextPage
	}
}

// cloneWorker pulls from the cloneTasksChan and runs cloneOrPullRepo for each task.
func cloneWorker(tasks <-chan cloneTask) {
	for task := range tasks {
		totalBar.UpdateTitle("Cloning " + task.repoURL)
		err := cloneOrPullRepo(task.repoURL, task.destDir, *timeoutFlag, task.defaultBranch)

		if err != nil {
			pterm.Warning.Printf("Failed to clone or update repository %s: %v\n", task.repoURL, err)
			continue
		}

		doneTasks++
		totalBar.Increment()
	}
}

// ... (cloneOrPullRepo, cloneWithTimeout, and pullWithTimeout functions remain unchanged)
func cloneOrPullRepo(url string, path string, timeoutFlag string, defaultBranch string) error {
	duration, err := time.ParseDuration(timeoutFlag)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	// Check if .git directory exists
	_, err = os.Stat(path + "/.git")
	if os.IsNotExist(err) { // If not exists, it is not a git repository, so clone.
		return cloneWithTimeout(ctx, url, path, defaultBranch)
	}

	// Else, it's already a repository, try pull.
	return pullWithTimeout(ctx, path, defaultBranch)
}

// cloneWithTimeout attempts to clone a repository at given url to a destination path, but will time out and abort the operation if it takes too long.
func cloneWithTimeout(ctx context.Context, url string, path string, defaultBranch string) error {
	ch := make(chan error)
	go func() {
		_, err := git.PlainClone(path, false, &git.CloneOptions{
			URL:           url,
			Progress:      os.Stdout,
			ReferenceName: plumbing.NewBranchReferenceName(defaultBranch),
			SingleBranch:  false,
		})
		ch <- err
	}()

	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		pterm.Error.Printf("Cloning %s timed out\n", url)
		return fmt.Errorf("cloning %s timed out", url) // If we timed out, return an error.
	}
}

func pullWithTimeout(ctx context.Context, path string, defaultBranch string) error {
	pterm.Info.Printf("Pulling %s\n", path)
	ch := make(chan error)
	go func() {
		r, err := git.PlainOpen(path)
		if err != nil {
			ch <- err
			return
		}
		w, err := r.Worktree()
		if err != nil {
			ch <- err
			return
		}

		// Fetch the latest commits from the origin remote
		err = r.Fetch(&git.FetchOptions{
			RemoteName: "origin",
			RefSpecs:   []config.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
			Force:      true,
		})
		if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			ch <- err
			return
		}

		// Gets the hash of the remote default branch
		ref, err := r.Reference(plumbing.NewRemoteReferenceName("origin", defaultBranch), true)
		if err != nil {
			ch <- err
			return
		}

		// Reset the current working directory to the fetched hash
		err = w.Reset(&git.ResetOptions{
			Commit: ref.Hash(),
			Mode:   git.HardReset,
		})
		ch <- err
	}()

	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		pterm.Error.Printf("Pulling in %s timed out\n", path)
		return fmt.Errorf("pulling in %s timed out", path)
	}
}
