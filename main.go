package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/pkg/errors"
)

const (
	fileDateLayout = "2006-01-02_15:04:05"
	cloningWorkers = 5
	maxPages       = 10
	perPage        = 100
	reposURL       = "https://api.github.com/orgs/%s/repos"
	programTimeout = 30 * time.Minute
)

func main() {
	org := os.Getenv("ORG")
	if org == "" {
		panic("ORG env expected")
	}

	githubToken := os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		panic("GITHUB_TOKEN env expected")
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), programTimeout)
	defer cancel()

	reposData, err := fetchReposData(ctx, org, githubToken)
	if err != nil {
		panic("could not fetch repos data:" + err.Error())
	}
	fmt.Printf("Data for %d repositories fetched in total\n", len(reposData))

	dirFilename := fmt.Sprintf("%s-archive-%s", org, time.Now().Format(fileDateLayout))
	err = os.Mkdir(dirFilename, os.ModePerm)
	if err != nil {
		panic("could not create directory:" + err.Error())
	}

	wg := &sync.WaitGroup{}
	storeReposResponses(wg, reposData, dirFilename)
	cloneRepos(ctx, wg, dirFilename, githubToken, reposData)

	fmt.Println("Waiting for workers to finish...")
	wg.Wait()

	fmt.Println("Preparing zip archive...")
	zipFile, err := os.OpenFile(dirFilename+".zip", os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		panic("could not open zip file:" + err.Error())
	}
	defer zipFile.Close()

	w := zip.NewWriter(zipFile)
	defer w.Close()
	err = fillZipWriter(dirFilename, w)
	if err != nil {
		panic("could not fill zip writer:" + err.Error())
	}

	err = os.RemoveAll(dirFilename)
	if err != nil {
		panic("could not remove working directory:" + err.Error())
	}

	fmt.Printf("Done in %s!\n", time.Since(start))
}

func fillZipWriter(dirFilename string, w *zip.Writer) error {
	return filepath.WalkDir(dirFilename, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		file, err := entry.Info()
		if err != nil {
			return err
		}
		if file.Mode()&os.ModeSymlink == os.ModeSymlink {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}

			header := &zip.FileHeader{
				Name:   path,
				Method: zip.Store,
			}
			header.SetMode(os.ModeSymlink)

			writer, err := w.CreateHeader(header)
			if err != nil {
				return err
			}

			_, err = writer.Write([]byte(linkTarget))
			if err != nil {
				return err
			}
			return nil
		}

		header, err := zip.FileInfoHeader(file)
		if err != nil {
			return err
		}
		header.Name, err = filepath.Rel(dirFilename, path)
		if err != nil {
			return err
		}

		writer, err := w.CreateHeader(header)
		if err != nil {
			return err
		}

		fileReader, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fileReader.Close()

		_, err = io.Copy(writer, fileReader)
		return err
	})
}

func cloneRepos(ctx context.Context, wg *sync.WaitGroup, dirFilename string, githubToken string, reposData []*MinimalRepository) {
	work := make(chan string)

	for i := range cloningWorkers {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			fmt.Printf("starting worker %d\n", i)
			for {
				select {
				case <-ctx.Done():
					fmt.Printf("context done for worker %d, %s\n", i, ctx.Err().Error())
					return
				case s, ok := <-work:
					if !ok {
						fmt.Printf("work done for worker %d\n", i)
						return
					}

					path := path.Base(s)
					path = strings.TrimSuffix(path, ".git")
					_, err := git.PlainCloneContext(ctx, dirFilename+"/"+path, false, &git.CloneOptions{
						URL: s,
						Auth: &githttp.BasicAuth{
							Username: "username",
							Password: githubToken,
						},
					})
					if err != nil {
						fmt.Printf("\nerror cloning %s:%s\n", s, err.Error())
					}
				}
			}
		}()
	}

	for i, repo := range reposData {
		work <- repo.CloneUrl
		fmt.Printf("cloning of '%s' requested, %d/%d\n", repo.Name, i+1, len(reposData))
	}
	close(work)
}

func storeReposResponses(wg *sync.WaitGroup, reposData []*MinimalRepository, dirFilename string) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		fmt.Println("saving fetched repositories responses to file...")
		j, err := json.MarshalIndent(reposData, "", "  ")
		if err != nil {
			panic("could not marshal repos:" + err.Error())
		}
		err = os.WriteFile(dirFilename+"/responses.json", j, os.ModePerm)
		if err != nil {
			panic("could not write to file:" + err.Error())
		}
		fmt.Println("fetched repositories responses saved to file")
	}()
}

func fetchReposData(ctx context.Context, org string, githubToken string) ([]*MinimalRepository, error) {
	url := fmt.Sprintf(reposURL, org)
	r, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrap(err, "could not create new http request")
	}

	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("Authorization", "Bearer "+githubToken)

	repos := []*MinimalRepository{}

	client := &http.Client{}
	for i := 1; i <= maxPages; i++ {
		select {
		case <-ctx.Done():
			return nil, errors.Wrap(err, "context finished")
		default:
			q := r.URL.Query()
			q.Set("per_page", strconv.Itoa(perPage))
			q.Set("page", strconv.Itoa(i))
			r.URL.RawQuery = q.Encode()

			fmt.Printf("fetching %d. batch\n", i)
			resp, err := client.Do(r)
			if err != nil {
				return nil, errors.Wrap(err, "could not do the request")
			}

			if resp.StatusCode != http.StatusOK {
				return nil, errors.Errorf("received invalid response code for batch %d:'%d'", i, resp.StatusCode)
			}

			respStr := []*MinimalRepository{}
			err = json.NewDecoder(resp.Body).Decode(&respStr)
			if err != nil {
				return nil, errors.Wrap(err, "could not decode response")
			}
			resp.Body.Close()

			fmt.Printf("fetched %d. batch with %d repos\n", i, len(respStr))
			repos = append(repos, respStr...)
			if len(respStr) < perPage {
				return repos, nil
			}
		}
	}
	return repos, nil
}
