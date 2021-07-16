package main

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/google/go-github/v36/github"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/oauth2"
)

// Migratron - safely migrate a repo to another org
// Allows one to safely open source a previously internal project by
// making it simple to sync the labels, issues, comments to a new
// project repo.

const (
	default_editor = "vim"
)

var (
	issue                                       int
	all                                         bool
	migratedToLabel, migratedFromLabel, ghLogin string

	badUriParts  = []string{"jira", "confluence.eng", "drive.google", "slack.com", "miro.com"}
	bannedLabels = []string{"migration/essential"}
	skipLabel    = "migration/selfservice"
)

type issueSyncRequest struct {
	number          int
	syncAssignee    bool
	syncLabels      bool
	collateComments bool
	body            string
	title           string
	fromRepo        string
	toRepo          string
}

func init() {
	cobra.OnInitialize(initConfig)

	migrateSingleIssueCmd.PersistentFlags().StringVar(&ghLogin, "login", "", "your github login")
	migrateSingleIssueCmd.PersistentFlags().StringVar(&migratedToLabel, "to-label", "migration/migrated", "label to denote an issue has been processed and migrated")
	migrateSingleIssueCmd.PersistentFlags().StringVar(&migratedToLabel, "from-label", "migration/imported", "label to denote an issue has been created as result of an import")
	migrateSingleIssueCmd.PersistentFlags().StringVar(&migratedToLabel, "from-label", "migration/imported", "label to denote an issue has been created as result of an import")

	migrateAllIssueCmd.PersistentFlags().StringVar(&ghLogin, "login", "", "your github login")
	migrateAllIssueCmd.PersistentFlags().StringVar(&migratedToLabel, "to-label", "migration/migrated", "label to denote an issue has been processed and migrated")
	migrateAllIssueCmd.PersistentFlags().StringVar(&migratedFromLabel, "from-label", "migration/imported", "label to denote an issue has been created as result of an import")
	migrateAllIssueCmd.PersistentFlags().StringVar(&migratedFromLabel, "from-label", "migration/imported", "label to denote an issue has been created as result of an import")

	RootCmd.AddCommand(IssuesCmd)
	IssuesCmd.AddCommand(migrateSingleIssueCmd)
	IssuesCmd.AddCommand(migrateAllIssueCmd)
}

func main() {
	RootCmd.Execute()
}

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "migratron",
	Short: "Tools for migrating repositories",
}

var IssuesCmd = &cobra.Command{
	Use:   "issues",
	Short: "Tools to migrate issues between repos",
}

var migrateSingleIssueCmd = &cobra.Command{
	Use:   "migrate <issue#>",
	Short: "Migrate a single issue",
	RunE:  migrateSingleIssue,
}

var migrateAllIssueCmd = &cobra.Command{
	Use:   "all",
	Short: "migrate all repo issues to the new repo",
	RunE:  migrateAllIssue,
}

// Migrate issues as a transaction to avoid any inconsistencies from manual copying
func migrateAllIssue(cmd *cobra.Command, args []string) error {
	if ghLogin == "" {
		return errors.New("--login must be set!")
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: viper.GetString("TOKEN")},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	repoParts := strings.Split(viper.GetString("FROM_REPO"), "/")
	if len(repoParts) < 2 || len(repoParts) > 2 {
		return fmt.Errorf("FROM_REPO env is not in org/repo format: %q", viper.GetString("FROM_REPO"))
	}
	toRepoParts := strings.Split(viper.GetString("TO_REPO"), "/")
	if len(repoParts) < 2 || len(repoParts) > 2 {
		return fmt.Errorf("TO_REPO env is not in org/repo format: %q", viper.GetString("TO_REPO"))
	}
	fromRepo := ghRepo{
		org:  repoParts[0],
		name: repoParts[1],
	}
	toRepo := ghRepo{
		org:  toRepoParts[0],
		name: toRepoParts[1],
	}

	issues, _, err := client.Issues.ListByRepo(ctx,
		repoParts[0],
		repoParts[1],
		&github.IssueListByRepoOptions{
			ListOptions: github.ListOptions{
				PerPage: 1000,
			},
			Sort:      "created",
			Direction: "desc",
		})
	if err != nil {
		return err
	}
OUTER:
	for _, i := range issues {
		if i.IsPullRequest() {
			continue
		}

		for _, l := range i.Labels {
			if *l.Name == skipLabel || *l.Name == migratedToLabel {
				cmd.Printf("skipped: %d\n", *i.Number)
				continue OUTER
			}
		}
		if err := migrateOne(ctx, cmd, i, client, toRepo, fromRepo); err != nil {
			return err
		}
	}

	cmd.Println("Completed all issues!")

	return nil
}

type ghRepo struct {
	org  string
	name string
}

// Migrate issues as a transaction to avoid any inconsistencies from manual copying
func migrateSingleIssue(cmd *cobra.Command, args []string) error {
	if ghLogin == "" {
		return errors.New("--login must be set!")
	}

	repoParts := strings.Split(viper.GetString("FROM_REPO"), "/")
	if len(repoParts) < 2 || len(repoParts) > 2 {
		return fmt.Errorf("FROM_REPO env is not in org/repo format: %q", viper.GetString("FROM_REPO"))
	}
	toRepoParts := strings.Split(viper.GetString("TO_REPO"), "/")
	if len(repoParts) < 2 || len(repoParts) > 2 {
		return fmt.Errorf("TO_REPO env is not in org/repo format: %q", viper.GetString("TO_REPO"))
	}

	fromRepo := ghRepo{
		org:  repoParts[0],
		name: repoParts[1],
	}
	toRepo := ghRepo{
		org:  toRepoParts[0],
		name: toRepoParts[1],
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: viper.GetString("TOKEN")},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	if len(args) == 0 {
		return errors.New("No issue number provided")
	}
	issue, err := strconv.Atoi(args[0])
	if err != nil {
		return err
	}

	ghIssue, _, err := client.Issues.Get(ctx, fromRepo.org, fromRepo.name, issue)
	if err != nil {
		return err
	}
	if ghIssue.IsPullRequest() {
		return errors.New("This is a PR, can not migrate")
	}

	for _, l := range ghIssue.Labels {
		if *l.Name == skipLabel {
			return errors.New("This issue has label migration/selfservice applied, exiting")
		}
	}
	if err := migrateOne(ctx, cmd, ghIssue, client, toRepo, fromRepo); err != nil {
		return err
	}

	return nil
}

func migrateOne(ctx context.Context, cmd *cobra.Command, issue *github.Issue, client *github.Client, to, from ghRepo) error {
	c, _, err := client.Issues.ListComments(ctx, from.org, from.name, *issue.Number, &github.IssueListCommentsOptions{})
	if err != nil {
		return err
	}
	cmd.Println("-------------------------------")
	cmd.Printf("Migrating Issue %d\nTitle: %q\nBody: %q\nURL: %s\n\n", *issue.Number, *issue.Title, *issue.Body, *issue.HTMLURL)
	// Import?
	importPrompt := promptui.Prompt{
		Label:     "Import Issue?",
		IsConfirm: true,
	}
	importIssue, _ := importPrompt.Run()
	if importIssue != "y" {
		return nil
	}

	req, err := generateIssueRequest(cmd, issue, c)
	if err != nil {
		return err
	}

	migrationPrompt := promptui.Prompt{
		Label:     "Migrate Resource?",
		IsConfirm: true,
	}
	m, err := migrationPrompt.Run()
	if err != nil {
		return err
	}
	if m != "y" {
		return nil
	}

	newIssue, _, err := client.Issues.Create(ctx, to.org, to.name, req)
	if err != nil {
		return err
	}
	finalIssue, _, err := client.Issues.Get(ctx, to.org, to.name, *newIssue.Number)
	if err != nil {
		return err
	}

	myUser, _, err := client.Users.Get(ctx, ghLogin)
	if err != nil {
		return err
	}
	commentBody := "Migrated to " + *finalIssue.HTMLURL + "."
	comment := github.IssueComment{
		Body: &commentBody,
		User: myUser,
	}
	_, _, err = client.Issues.CreateComment(ctx, from.org, from.name, *issue.Number, &comment)
	if err != nil {
		return err
	}

	_, _, err = client.Issues.AddLabelsToIssue(ctx, from.org, from.name, *issue.Number, []string{migratedToLabel})
	if err != nil {
		return err
	}

	cmd.Print("\n-------------------------------\n")
	cmd.Printf("Successfully migrated issue %d to:\n", issue.Number)
	cmd.Println(*finalIssue.HTMLURL)
	cmd.Printf("Please review each issue for accuracy")
	cmd.Print("\n-------------------------------\n\n")

	return nil
}

func scanForInternal(s *string) bool {
	for _, b := range badUriParts {
		if strings.Contains(*s, b) {
			return true
		}
	}
	return false
}

func generateIssueRequest(cmd *cobra.Command, issue *github.Issue, comments []*github.IssueComment) (*github.IssueRequest, error) {
	req := &github.IssueRequest{
		Title: issue.Title,
		Body:  issue.Body,
	}

	// Edit the title
	editTitlePrompt := promptui.Prompt{
		Label:     "Edit Title",
		IsConfirm: true,
	}
	if scanForInternal(issue.Title) {
		editTitlePrompt.Label = "Issue Title Alert! Internal Terms found in title. Please be sure to edit!"
	}
	editTitle, _ := editTitlePrompt.Run()
	if editTitle == "y" {
		updateTitlePrompt := promptui.Prompt{
			Label:     "Update Title",
			Default:   *issue.Title,
			AllowEdit: true,
		}
		u, err := updateTitlePrompt.Run()
		if err != nil {
			return nil, err
		}
		req.Title = &u
	}

	// Edit the body
	editBodyPrompt := promptui.Prompt{
		Label:     "Edit Body",
		IsConfirm: true,
	}
	if scanForInternal(issue.Body) {
		editBodyPrompt.Label = "Issue Body Alert! Internal Terms found in body. Please be sure to edit!"
	}
	editBody, _ := editBodyPrompt.Run()
	if editBody == "y" {
		bodyBytes, err := editBodyVim("migratron.*.body.txt", *issue.Body)
		if err != nil {
			return nil, err
		}
		bodyString := string(bodyBytes)
		req.Body = &bodyString
	}

	// Sync labels
	syncLabelPrompt := promptui.Prompt{
		Label:     "Sync Labels",
		IsConfirm: true,
	}
	syncLabels, _ := syncLabelPrompt.Run()
	if syncLabels == "y" {
		synced := assertAndSyncLabels(issue.Labels)
		req.Labels = &synced
	}

	// Sync labels
	collateCommentsPrompt := promptui.Prompt{
		Label:     "Collate Comments",
		IsConfirm: true,
	}
	collate, _ := collateCommentsPrompt.Run()
	if collate == "y" {
		collated, err := collateComments(cmd, comments)
		if err != nil {
			return nil, err
		}
		if len(collated) > 0 {
			updatedBody := *req.Body + "\n### Collated Context\n" + string(collated)
			req.Body = &updatedBody
		}
	}

	return req, nil
}

func assertAndSyncLabels(labels []*github.Label) []string {
	toLabels := []string{migratedFromLabel}
	for _, l := range labels {
		for _, banned := range bannedLabels {
			if *l.Name == banned {
				continue
			}
		}
		toLabels = append(toLabels, *l.Name)
	}
	return toLabels
}

// collateComments
func collateComments(cmd *cobra.Command, comments []*github.IssueComment) (cBytes []byte, err error) {
	var collated, addComment string
	for _, comment := range comments {
		if scanForInternal(comment.Body) {
			cmd.Printf("\nAlert! Internal Terms found in comment. Forcing edit!")
		}

		cmd.Printf("\nComment: %s\n", *comment.Body)
		addCommentPrompt := promptui.Prompt{
			Label:     "Add Comment",
			IsConfirm: true,
		}
		if scanForInternal(comment.Body) {
			addCommentPrompt.Label = "Comment Alert! Internal Terms found in comment. Please be sure to edit!"
		}
		addComment, _ = addCommentPrompt.Run()
		if addComment != "y" {
			continue
		}

		commentMetadata := fmt.Sprintf("\nContext from %s", comment.CreatedAt.Format("2006-01-02 15:04:05"))
		commentMetadata = commentMetadata + "\n" + "User: " + *comment.User.Login
		collated = collated + "\n" + commentMetadata + "\n" + *comment.Body + "\n"
	}
	cBytes, err = editBodyVim("migratron.*.collate.txt", collated)
	if err != nil {
		return
	}

	return
}

func editBodyVim(filename, body string) (file []byte, err error) {
	tmpfile, err := ioutil.TempFile("", filename)
	if err != nil {
		return
	}
	defer os.Remove(tmpfile.Name())
	if _, err = tmpfile.Write([]byte(body)); err != nil {
		tmpfile.Close()
		return
	}
	if err = tmpfile.Close(); err != nil {
		return
	}

	cmd := editorCmd(tmpfile.Name())
	err = cmd.Run()
	if err != nil {
		return
	}

	file, err = ioutil.ReadFile(tmpfile.Name())
	if err != nil {
		return
	}

	return
}

func editorCmd(filename string) *exec.Cmd {
	editorPath := os.Getenv("EDITOR")
	if editorPath == "" {
		editorPath = default_editor
	}
	editor := exec.Command(editorPath, filename)

	editor.Stdin = os.Stdin
	editor.Stdout = os.Stdout
	editor.Stderr = os.Stderr

	return editor
}

func initConfig() {
	viper.SetEnvPrefix("MIGRATRON")
	viper.BindEnv("TOKEN")
	viper.BindEnv("FROM_REPO")
	viper.BindEnv("TO_REPO")

	viper.AutomaticEnv()
}
