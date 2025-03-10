package extractor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/text/language"
	"golang.org/x/text/search"

	"github.com/Techloopio/extractor_tool/commit"
	"github.com/Techloopio/extractor_tool/languagedetection"
	"github.com/Techloopio/extractor_tool/librarydetection"
	"github.com/Techloopio/extractor_tool/librarydetection/languages"
	"github.com/Techloopio/extractor_tool/obfuscation"
	"github.com/Techloopio/extractor_tool/ui"
)

// RepoExtractor is responsible for all parts of repo extraction process
// Including cloning the repo, processing the commits and uploading the results
type RepoExtractor struct {
	RepoPath                   string
	OutputPath                 string
	GitPath                    string
	HashImportant              bool
	SkipLibraries              bool // If it is false there is no library detection.
	UserEmails                 []string
	TimeLimit                  time.Duration // If set the extraction will be stopped after the given time limit and the partial result will be uploaded
	Seed                       []string
	repo                       *repo
	userCommits                []*commit.Commit // Commits which are belong to user (from selected emails)
	commitPipeline             chan commit.Commit
	libraryExtractionCompleted chan bool
}

// Extract a single repo in the path
func (r *RepoExtractor) Extract() error {
	var ctx context.Context
	var cancel context.CancelFunc

	if r.TimeLimit.Seconds() != 0.0 {
		ctx, cancel = context.WithTimeout(context.Background(), r.TimeLimit)
		defer cancel()
	} else {
		ctx = context.Background()
	}

	err := r.initRepo()
	if err != nil {
		fmt.Println("Cannot init extractor_tool. Error: ", err.Error())
		return err
	}

	// For library detection
	r.initAnalyzers()

	err = r.analyseCommits(ctx)
	if err != nil {
		return err
	}
	go r.analyseLibraries(ctx)

	err = r.export()
	if err != nil {
		fmt.Println("Couldn't export commits to export. Error:", err.Error())
		return err
	}

	return nil
}

// Creates Repo struct
func (r *RepoExtractor) initRepo() error {
	fmt.Println("Initializing repository")

	r.commitPipeline = make(chan commit.Commit)
	r.libraryExtractionCompleted = make(chan bool)
	cmd := exec.Command(r.GitPath,
		"config",
		"--get",
		"remote.origin.url",
	)
	cmd.Dir = r.RepoPath

	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Println("Cannot get remote.origin.url. Use directory path to get repo name.")
	}

	repoName := ""
	remoteOrigin := string(out)
	remoteOrigin = strings.TrimRight(remoteOrigin, "\r\n")
	remoteOrigin = strings.TrimRight(remoteOrigin, "\n")

	repoName = r.GetRepoName(remoteOrigin)

	r.repo = &repo{
		RepoName:        repoName,
		Emails:          []string{},
		SuggestedEmails: []string{}, // TODO implement
	}
	return nil
}

// GetRepoName gets the repo name in the following format:
// in case of headless: "owner_name/repo_name"
// in case of interactive mode: "repo_name"
func (r *RepoExtractor) GetRepoName(remoteOrigin string) string {
	// If remoteOrigin is empty fall back to the repos path. It can happen in interactive mode
	if remoteOrigin == "" {
		parts := strings.Split(r.RepoPath, "/")
		return parts[len(parts)-1]
	}
	repoName := ""
	remoteOrigin = strings.TrimSuffix(remoteOrigin, ".git")
	if strings.Contains(remoteOrigin, "http") {
		// Cloned using http
		parts := strings.Split(remoteOrigin, "/")
		repoName = parts[len(parts)-1]
	} else {
		// Cloned using ssh
		parts := strings.Split(remoteOrigin, ":")
		repoName = parts[len(parts)-1]
		parts = strings.Split(repoName, "/")
		repoName = parts[len(parts)-1]
	}

	return repoName
}

func (r *RepoExtractor) initAnalyzers() {
	librarydetection.AddAnalyzer("Go", languages.NewGoAnalyzer())
	librarydetection.AddAnalyzer("C", languages.NewCAnalyzer())
	librarydetection.AddAnalyzer("C++", languages.NewCppAnalyzer())
	librarydetection.AddAnalyzer("C#", languages.NewCSharpAnalyzer())
	librarydetection.AddAnalyzer("Java", languages.NewJavaAnalyzer())
	librarydetection.AddAnalyzer("JavaScript", languages.NewJavaScriptAnalyzer())
	librarydetection.AddAnalyzer("Kotlin", languages.NewKotlinAnalyzer())
	librarydetection.AddAnalyzer("TypeScript", languages.NewTypeScriptAnalyzer())
	librarydetection.AddAnalyzer("Perl", languages.NewPerlAnalyzer())
	librarydetection.AddAnalyzer("PHP", languages.NewPHPAnalyzer())
	librarydetection.AddAnalyzer("Python", languages.NewPythonScriptAnalyzer())
	librarydetection.AddAnalyzer("Ruby", languages.NewRubyScriptAnalyzer())
	librarydetection.AddAnalyzer("Swift", languages.NewSwiftAnalyzer())
}

// Creates commits
func (r *RepoExtractor) analyseCommits(ctx context.Context) error {
	fmt.Println("Analysing commits")

	var commits []*commit.Commit
	commits, err := r.getCommits(ctx)
	userCommits := make([]*commit.Commit, 0, len(commits))
	if len(commits) == 0 {
		return nil
	}
	if err != nil {
		return err
	}

	allEmails := getAllEmails(commits)
	selectedEmails := make(map[string]bool)

	if len(r.UserEmails) == 0 {
		selectedEmailsWithNames := ui.SelectEmail(allEmails)
		emails, emailsMap := getEmailsWithoutNames(selectedEmailsWithNames)
		r.repo.Emails = append(r.repo.Emails, emails...)
		for mail := range emailsMap {
			selectedEmails[mail] = true
		}
	} else {
		r.repo.Emails = append(r.repo.Emails, r.UserEmails...)
		for _, email := range r.UserEmails {
			selectedEmails[email] = true
		}
	}

	// Only consider commits for user
	for _, v := range commits {
		if _, ok := selectedEmails[v.AuthorEmail]; ok {
			userCommits = append(userCommits, v)
		}
	}

	r.userCommits = userCommits
	return nil
}

func (r *RepoExtractor) getCommits(ctx context.Context) ([]*commit.Commit, error) {
	jobs := make(chan *req)
	results := make(chan []*commit.Commit)
	noMoreChan := make(chan bool)
	for w := 0; w < runtime.NumCPU(); w++ {
		go func() {
			err := r.commitWorker(w, jobs, results, noMoreChan)
			if err != nil {
				fmt.Println("Error during getting commits. Error: " + err.Error())
			}
		}()
	}

	// launch initial jobs
	lastOffset := 0
	step := 1000
	for x := 0; x < runtime.NumCPU(); x++ {
		jobs <- &req{
			Limit:  step,
			Offset: x * step,
		}
		lastOffset = step * x
	}

	var commits []*commit.Commit
	workersReturnedNoMore := 0

	var pb ui.ProgressBar
	numberOfCommits := r.getNumberOfCommits()
	if numberOfCommits > 0 {
		pb = ui.NewProgressBar(numberOfCommits)
	} else {
		pb = ui.NilProgressBar()
	}

	func() {
		for {
			select {
			case res := <-results:
				lastOffset += step
				jobs <- &req{
					Limit:  step,
					Offset: lastOffset,
				}
				commits = append(commits, res...)
				pb.SetCurrent(len(commits))
			case <-noMoreChan:
				workersReturnedNoMore++
				if workersReturnedNoMore == runtime.NumCPU() {
					close(jobs)
					return
				}
			case <-ctx.Done():
				fmt.Println("Time limit exceeded. Couldn't get all the commits.")
				close(jobs)
				return
			}
		}
	}()
	pb.Finish()

	return commits, nil
}

func getAllEmails(commits []*commit.Commit) []string {
	allEmails := make([]string, 0, len(commits))
	emails := make(map[string]bool) // To prevent duplicates
	for _, v := range commits {
		if _, ok := emails[v.AuthorEmail]; !ok {
			emails[v.AuthorEmail] = true
			allEmails = append(allEmails, fmt.Sprintf("%s -> %s", v.AuthorName, v.AuthorEmail))
		}
	}
	return allEmails
}

func getEmailsWithoutNames(emails []string) ([]string, map[string]bool) {
	emailsWithoutNames := make(map[string]bool, len(emails))
	emailsWithoutNamesArray := make([]string, len(emails))
	for i, selectedEmail := range emails {
		fields := strings.Split(selectedEmail, " -> ")
		// TODO handle authorName being empty
		if len(fields) > 0 {
			emailsWithoutNames[fields[1]] = true
			emailsWithoutNamesArray[i] = fields[1]
		}
	}
	return emailsWithoutNamesArray, emailsWithoutNames
}

func (r *RepoExtractor) getNumberOfCommits() int {
	cmd := exec.Command(r.GitPath,
		"--no-pager",
		"log",
		"--all",
		"--no-merges",
		"--pretty=oneline",
	)
	cmd.Dir = r.RepoPath
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Println("Cannot get number of commits. Cannot show progress bar. Error: " + err.Error())
		return 0
	}
	return strings.Count(string(stdout), "\n")
}

// commitWorker get commits from git
func (r *RepoExtractor) commitWorker(w int, jobs <-chan *req, results chan<- []*commit.Commit, noMoreChan chan<- bool) error {
	for v := range jobs {
		var commits []*commit.Commit

		cmd := exec.Command(r.GitPath,
			"log",
			"--numstat",
			"--all",
			fmt.Sprintf("--skip=%d", v.Offset),
			fmt.Sprintf("--max-count=%d", v.Limit),
			"--pretty=format:|||BEGIN|||%H|||SEP|||%an|||SEP|||%ae|||SEP|||%ad",
			"--no-merges",
		)
		cmd.Dir = r.RepoPath
		stdout, err := cmd.StdoutPipe()
		if nil != err {
			fmt.Println("Cannot create pipe.")
			return err
		}
		if err := cmd.Start(); err != nil {
			fmt.Println("Error during execution of Git command.")
			return err
		}

		// parse the output into stats
		scanner := bufio.NewScanner(stdout)
		currentLine := 0
		var currectCommit *commit.Commit
		for scanner.Scan() {
			m := scanner.Text()
			currentLine++
			if m == "" {
				continue
			}
			if strings.HasPrefix(m, "|||BEGIN|||") {
				// we reached a new commit
				// save the existing
				if currectCommit != nil {
					commits = append(commits, currectCommit)
				}

				// and add new one commit
				m = strings.Replace(m, "|||BEGIN|||", "", 1)
				bits := strings.Split(m, "|||SEP|||")
				changedFiles := []*commit.ChangedFile{}
				dateStr := ""
				t, err := time.Parse("Mon Jan 2 15:04:05 2006 -0700", bits[3])
				if err == nil {
					dateStr = t.Format("2006-01-02 15:04:05 -0700")
				} else {
					fmt.Println("Cannot convert date. Expected date format: Mon Jan 2 15:04:05 2006 -0700. Got: " + bits[3])
				}
				currectCommit = &commit.Commit{
					Hash:         bits[0],
					AuthorName:   bits[1],
					AuthorEmail:  bits[2],
					Date:         dateStr,
					ChangedFiles: changedFiles,
				}
				continue
			}

			bits := strings.Fields(m)

			insertionsString := bits[0]
			if insertionsString == "-" {
				insertionsString = "0"
			}
			insertions, err := strconv.Atoi(insertionsString)
			if err != nil {
				fmt.Println("Cannot convert the following into integer: " + insertionsString)
				return err
			}

			deletionsString := bits[1]
			if deletionsString == "-" {
				deletionsString = "0"
			}
			deletions, err := strconv.Atoi(deletionsString)
			if err != nil {
				fmt.Println("Cannot convert the following into integer: " + deletionsString)
				return err
			}

			fileName := bits[2]
			// it is a rename, skip
			if strings.Contains("=>", fileName) {
				continue
			}

			changedFile := &commit.ChangedFile{
				Path:       bits[2],
				Insertions: insertions,
				Deletions:  deletions,
			}

			if currectCommit == nil {
				// TODO maybe skip? does this break anything?
				return errors.New("did not expect current commit to be null")
			}

			if currectCommit.ChangedFiles == nil {
				// TODO maybe skip? does this break anything?
				return errors.New("did not expect current commit changed files to be null")
			}

			currectCommit.ChangedFiles = append(currectCommit.ChangedFiles, changedFile)
		}

		// last commit will not get appended otherwise
		// because scanner is not returning anything
		if currectCommit != nil {
			commits = append(commits, currectCommit)
		}

		if len(commits) == 0 {
			noMoreChan <- true
			return nil
		}
		results <- commits
	}
	return nil
}

func (r *RepoExtractor) analyseLibraries(ctx context.Context) {
	fmt.Println("Analysing libraries")
	defer func() {
		r.libraryExtractionCompleted <- true
	}()

	jobs := make(chan *commit.Commit, len(r.userCommits))
	results := make(chan bool, len(r.userCommits))
	// Analyse libraries for every commit
	for w := 1; w <= runtime.NumCPU(); w++ {
		go r.libraryWorker(ctx, jobs, results)
	}
	for _, v := range r.userCommits {
		jobs <- v
	}
	close(jobs)
	pb := ui.NewProgressBar(len(r.userCommits))
	for a := 1; a <= len(r.userCommits); a++ {
		<-results
		pb.Inc()
	}
	pb.Finish()
}

func (r *RepoExtractor) getFileContent(commitHash, filePath string) ([]byte, error) {
	cmd := exec.Command(r.GitPath,
		"--no-pager",
		"show",
		fmt.Sprintf("%s:%s", commitHash, filePath),
	)
	cmd.Dir = r.RepoPath
	var err error
	fileContents, err := cmd.CombinedOutput()
	if err != nil {
		searchString1 := fmt.Sprintf("Path '%s' does not exist in '%s'", filePath, commitHash)
		searchString2 := fmt.Sprintf("Path '%s' exists on disk, but not in '%s'", filePath, commitHash)
		// Ignore case is needed because on windows error message starts with lowercase letter, in other systems it starts with uppercase letter
		stringSearcher := search.New(language.English, search.IgnoreCase)
		// means the file was deleted, skip
		start, end := stringSearcher.IndexString(string(fileContents), searchString1)
		if start != -1 && end != -1 {
			return []byte{}, nil
		}
		start, end = stringSearcher.IndexString(string(fileContents), searchString2)
		if start != -1 && end != -1 {
			return []byte{}, nil
		}
		return nil, err
	}
	return fileContents, nil
}

func (r *RepoExtractor) libraryWorker(ctx context.Context, commits <-chan *commit.Commit, results chan<- bool) error {
	languageAnalyzer := languagedetection.NewLanguageAnalyzer()
	hasTimeout := false
	for commitToAnalyse := range commits {
		c := commit.Commit{
			ChangedFiles: commitToAnalyse.ChangedFiles,
			Libraries:    make(map[string][]string),
		}
		c.Hash = commitToAnalyse.Hash
		c.AuthorEmail = commitToAnalyse.AuthorEmail
		c.AuthorName = commitToAnalyse.AuthorName
		c.Date = commitToAnalyse.Date
		libraries := map[string][]string{}
		for n, fileChange := range commitToAnalyse.ChangedFiles {
			select {
			case <-ctx.Done():
				if !hasTimeout {
					hasTimeout = true
					fmt.Println("Time limit exceeded. Couldn't analyze all the commits.")
				}
				c.Libraries = libraries
				r.commitPipeline <- c
				results <- true
				continue
			default:
			}

			lang := ""
			var fileContents []byte
			fileContents = nil

			extension := filepath.Ext(fileChange.Path)
			if extension == "" {
				continue
			}
			// remove the trailing dot
			extension = extension[1:]

			if languageAnalyzer.ShouldUseFile(extension) {
				var err error
				if fileContents == nil {
					fileContents, err = r.getFileContent(commitToAnalyse.Hash, fileChange.Path)
					if err != nil {
						continue
					}
				}
				lang = languageAnalyzer.DetectLanguageFromFile(fileChange.Path, fileContents)
			} else {
				lang = languageAnalyzer.DetectLanguageFromExtension(extension)
			}

			// We don't know extension, nothing to do
			if lang == "" {
				continue
			}
			c.ChangedFiles[n].Language = lang
			if !r.SkipLibraries {
				analyzer, err := librarydetection.GetAnalyzer(lang)
				if err != nil {
					continue
				}
				if fileContents == nil {
					fileContents, err = r.getFileContent(commitToAnalyse.Hash, fileChange.Path)
					if err != nil {
						continue
					}
				}
				fileLibraries, err := analyzer.ExtractLibraries(string(fileContents))
				if err != nil {
					fmt.Printf("error extracting libraries for %s: %s \n", lang, err.Error())
				}
				for index, fileLibrary := range fileLibraries {
					fileLibraries[index] = strings.Replace(fileLibrary, "../", "", -1)
				}
				if libraries[lang] == nil {
					libraries[lang] = make([]string, 0)
				}
				libraries[lang] = append(libraries[lang], fileLibraries...)
			}
		}
		c.Libraries = libraries
		r.commitPipeline <- c
		results <- true
	}
	return nil
}

func getStartOfDayFromStringDate(dateString string) time.Time {
	commitDate, _ := time.Parse("2006-01-02 15:04:05 -0700", dateString)
	return time.Date(commitDate.Year(), commitDate.Month(), commitDate.Day(), 0, 0, 0, 0, time.UTC)
}

func contains(slice []string, value string) bool {
	for _, sliceItem := range slice {
		if sliceItem == value {
			return true
		}
	}
	return false
}

func commitContainsExistingDate(slice []commit.OptimizedCommitForExport, value string) (bool, int) {
	for index, sliceItem := range slice {
		if sliceItem.Date == value {
			return true, index
		}
	}
	return false, -1
}

func removeDuplicateStrings(slice []string) []string {
	allKeys := make(map[string]bool)
	list := []string{}
	for _, item := range slice {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
}

func addUniqueEmailToCommitAuthorEmailsSlice(slice []string, email string) []string {
	if emailIsNotUnique := contains(slice, email); !emailIsNotUnique {
		slice = append(slice, email)
	}

	return slice
}

func getCommitJSonSuffix(commitSliceLength int, commitIndex int) string {
	if commitIndex < commitSliceLength-1 {
		return ","
	}

	return ""
}

// Writes result to the file
func (r *RepoExtractor) export() error {
	fmt.Println("Creating export at: " + r.OutputPath)

	repoDataPath := r.OutputPath + "_techloop.json"
	// Remove old files
	os.Remove(repoDataPath)

	// Create directory
	directories := strings.Split(r.OutputPath, string(os.PathSeparator))
	err := os.MkdirAll(strings.Join(directories[:len(directories)-1], string(os.PathSeparator)), 0755)
	if err != nil {
		log.Println("Cannot create directory. Error:", err.Error())
	}

	file, err := os.Create(repoDataPath)
	if err != nil {
		return err
	}

	w := bufio.NewWriter(file)
	fmt.Fprintln(w, "[")
	var preparedCommitsDataForExport []commit.OptimizedCommitForExport

loop:
	for {
		select {
		case commitFromPipeline := <-r.commitPipeline:
			commitDateStartHour := getStartOfDayFromStringDate(commitFromPipeline.Date)

			var commitLanguages []string
			var commitInsertions, commitDeletions int

			for _, commitChangedFile := range commitFromPipeline.ChangedFiles {
				if !contains(commitLanguages, commitChangedFile.Language) && commitChangedFile.Language != "" {
					commitLanguages = append(commitLanguages, commitChangedFile.Language)
				}
				commitInsertions += commitChangedFile.Insertions
				commitDeletions += commitChangedFile.Deletions
			}

			if _, index := commitContainsExistingDate(preparedCommitsDataForExport, commitDateStartHour.String()); index > -1 {
				newLibraries := preparedCommitsDataForExport[index].Libraries

				for newLibraryKey, newLibrary := range commitFromPipeline.Libraries {
					if _, currentLibraryExists := newLibraries[newLibraryKey]; currentLibraryExists {
						for _, libraryItem := range newLibrary {
							if !contains(newLibraries[newLibraryKey], libraryItem) {
								newLibraries[newLibraryKey] = append(newLibraries[newLibraryKey], libraryItem)
							}
						}
					} else {
						preparedCommitsDataForExport[index].Libraries[newLibraryKey] = removeDuplicateStrings(newLibrary)
					}
				}
				preparedCommitsDataForExport[index].Commits += 1
				preparedCommitsDataForExport[index].Deletions += commitDeletions
				preparedCommitsDataForExport[index].Insertions += commitInsertions
				preparedCommitsDataForExport[index].Libraries = newLibraries
				preparedCommitsDataForExport[index].AuthorEmails = addUniqueEmailToCommitAuthorEmailsSlice(preparedCommitsDataForExport[index].AuthorEmails, commitFromPipeline.AuthorEmail)

			} else {
				librariesWithoutDuplicity := make(map[string][]string)
				for libraryKey, library := range commitFromPipeline.Libraries {
					librariesWithoutDuplicity[libraryKey] = removeDuplicateStrings(library)
				}
				var authorEmails []string
				authorEmails = append(authorEmails, commitFromPipeline.AuthorEmail)
				optimizedCommit := commit.OptimizedCommitForExport{
					AuthorEmails: authorEmails,
					Date:         commitDateStartHour.String(),
					Languages:    commitLanguages,
					Libraries:    librariesWithoutDuplicity,
					Insertions:   commitInsertions,
					Deletions:    commitDeletions,
					Commits:      1,
				}

				if r.HashImportant {
					obfuscation.Obfuscate(&optimizedCommit)
				}
				preparedCommitsDataForExport = append(preparedCommitsDataForExport, optimizedCommit)
			}

		case <-r.libraryExtractionCompleted:
			break loop
		}
	}

	sort.Slice(preparedCommitsDataForExport, func(i, j int) bool {
		return preparedCommitsDataForExport[i].Date < preparedCommitsDataForExport[j].Date
	})

	for preparedCommitsDataForExportItemIndex, preparedCommitsDataForExportItem := range preparedCommitsDataForExport {
		commitData, err := json.Marshal(preparedCommitsDataForExportItem)
		if err != nil {
			fmt.Printf("Couldn't write commit day data to file. CommitDate: %s Error: %s", preparedCommitsDataForExportItem.Date, err.Error())
			continue
		}

		fmt.Fprintln(w, string(commitData)+getCommitJSonSuffix(len(preparedCommitsDataForExport), preparedCommitsDataForExportItemIndex))
	}
	fmt.Fprintln(w, "]")
	w.Flush() // important
	file.Close()

	fmt.Println("Exported!")
	fmt.Printf("File is located in folder export (%v)\n", repoDataPath)
	return nil
}

type repo struct {
	RepoName        string   `json:"repo"`
	Emails          []string `json:"emails"`
	SuggestedEmails []string `json:"suggestedEmails"`
}

type req struct {
	Limit  int
	Offset int
}
