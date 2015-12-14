// Go Package Store displays updates for the Go packages in your GOPATH.
package main

import (
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/shurcooL/Go-Package-Store/internal/util"
	"github.com/shurcooL/Go-Package-Store/presenter"
	"github.com/shurcooL/Go-Package-Store/repo"
	"github.com/shurcooL/go-goon"
	"github.com/shurcooL/go/exp/14"
	"github.com/shurcooL/go/gists/gist7480523"
	"github.com/shurcooL/go/gists/gist7651991"
	"github.com/shurcooL/go/gists/gist7802150"
	"github.com/shurcooL/go/gzip_file_server"
	"github.com/shurcooL/go/u/u4"
	"github.com/shurcooL/gostatus/status"
	"github.com/shurcooL/httpfs/html/vfstemplate"
	"golang.org/x/net/websocket"
)

func commonHead(w io.Writer) error {
	data := struct {
		Production bool
		HTTPAddr   string
	}{
		Production: production,
		HTTPAddr:   *httpFlag,
	}
	return t.ExecuteTemplate(w, "head.html.tmpl", data)
}
func commonTail(w io.Writer) error {
	return t.ExecuteTemplate(w, "tail.html.tmpl", nil)
}

// shouldPresentUpdate determines if the given goPackage should be presented as an available update.
// It checks that the Go package is on default branch, does not have a dirty working tree, and does not have the remote revision.
func shouldPresentUpdate(goPackage *gist7480523.GoPackage) bool {
	return status.PlumbingPresenterV2(goPackage)[:3] == "  +" // Ignore stash.
}

// writeRepoHTML writes a <div> presentation for an available update.
func writeRepoHTML(w http.ResponseWriter, repoPresenter presenter.Presenter) {
	err := t.ExecuteTemplate(w, "repo.html.tmpl", repoPresenter)
	if err != nil {
		log.Println("t.ExecuteTemplate:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var (
	// goPackages is a cached list of Go packages to work with.
	goPackages exp14.GoPackageList

	goPackages2 <-chan importPathRevision

	universe *goUniverse = newGoUniverse()

	// updater is set based on the source of Go packages. If nil, it means
	// we don't have support to update Go packages from the current source.
	// It's used to update repos in the backend, and to disable the frontend UI
	// for updating packages.
	updater repo.Updater
)

type updateRequest struct {
	importPathPattern string
	resultChan        chan error
}

var updateRequestChan = make(chan updateRequest)

// updateWorker is a sequential updater of Go packages. It does not update them in parallel
// to avoid race conditions or other problems, since `go get -u` does not seem to protect against that.
func updateWorker() {
	for updateRequest := range updateRequestChan {
		err := updater.Update(updateRequest.importPathPattern)
		updateRequest.resultChan <- err
		fmt.Println("\nDone.")
	}
}

// Handler for update requests.
func updateHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		return
	}

	updateRequest := updateRequest{
		importPathPattern: req.PostFormValue("import_path_pattern"),
		resultChan:        make(chan error),
	}
	updateRequestChan <- updateRequest

	err := <-updateRequest.resultChan
	_ = err // TODO: Maybe display error in frontend. For now, don't do anything.
}

// Main index page handler.
func mainHandler(w http.ResponseWriter, req *http.Request) {
	if err := loadTemplates(); err != nil {
		fmt.Fprintln(w, "loadTemplates:", err)
		return
	}

	started := time.Now()

	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	_ = commonHead(w)
	defer func() { _ = commonTail(w) }()

	flusher := w.(http.Flusher)
	flusher.Flush()

	fmt.Printf("Part 1: %v ms.\n", time.Since(started).Seconds()*1000)

	// Calculate the list of all Go packages (grouped by rootPath).
	var goPackagesInRepo = make(map[string][]*gist7480523.GoPackage) // Map key is rootPath.
	gist7802150.MakeUpdated(goPackages)
	fmt.Printf("Part 1b: %v ms.\n", time.Since(started).Seconds()*1000)
	if true {
		for _, goPackage := range goPackages.List() {
			if rootPath := util.GetRootPath(goPackage); rootPath != "" {
				goPackagesInRepo[rootPath] = append(goPackagesInRepo[rootPath], goPackage)
			}
		}
	} else {
		inChan := make(chan interface{})
		go func() { // This needs to happen in the background because sending input will be blocked on reading output.
			for _, goPackage := range goPackages.List() {
				inChan <- goPackage
			}
			close(inChan)
		}()
		reduceFunc := func(in interface{}) interface{} {
			goPackage := in.(*gist7480523.GoPackage)
			if rootPath := util.GetRootPath(goPackage); rootPath != "" {
				return gist7480523.NewGoPackageRepo(rootPath, []*gist7480523.GoPackage{goPackage})
			}
			return nil
		}
		outChan := gist7651991.GoReduce(inChan, 64, reduceFunc)
		for out := range outChan {
			repo := out.(gist7480523.GoPackageRepo)
			goPackagesInRepo[repo.RootPath()] = append(goPackagesInRepo[repo.RootPath()], repo.GoPackages()[0])
		}
	}

	goon.DumpExpr(len(goPackages.List()))
	goon.DumpExpr(len(goPackagesInRepo))

	universe.Wait()
	goon.DumpExpr(len(universe.repos))

	fmt.Printf("Part 2: %v ms.\n", time.Since(started).Seconds()*1000)

	updatesAvailable := 0

	inChan := make(chan interface{})
	go func() { // This needs to happen in the background because sending input will be blocked on reading output.
		for rootPath, goPackages := range goPackagesInRepo {
			inChan <- gist7480523.NewGoPackageRepo(rootPath, goPackages)
		}
		close(inChan)
	}()
	reduceFunc := func(in interface{}) interface{} {
		repo := in.(gist7480523.GoPackageRepo)

		goPackage := repo.GoPackages()[0]
		goPackage.UpdateVcsFields()

		if !shouldPresentUpdate(goPackage) {
			return nil
		}
		repoPresenter := presenter.New(&repo)
		return repoPresenter
	}
	outChan := gist7651991.GoReduce(inChan, 8, reduceFunc)

	for out := range outChan {
		started2 := time.Now()

		repoPresenter := out.(presenter.Presenter)

		updatesAvailable++
		writeRepoHTML(w, repoPresenter)

		flusher.Flush()

		fmt.Printf("Part 2b: %v ms.\n", time.Since(started2).Seconds()*1000)

		/*log.Println("WriteRepoHtml")
		goon.DumpExpr(repoPresenter.Repo().ImportPathPattern())
		goon.DumpExpr(repoPresenter.Repo().ImportPaths())
		goon.DumpExpr(len(repoPresenter.Repo().GoPackages()))
		goon.DumpExpr(repoPresenter.Repo().GoPackages()[0].Bpkg.ImportPath)
		goon.DumpExpr(repoPresenter.Repo().GoPackages()[0].Dir.Repo.VcsLocal.LocalRev)
		goon.DumpExpr(repoPresenter.Repo().GoPackages()[0].Dir.Repo.VcsRemote.RemoteRev)
		goon.DumpExpr(repoPresenter.HomePage())
		goon.DumpExpr(repoPresenter.Image())
		var changes []presenter.Change
		if changesChan := repoPresenter.Changes(); changesChan != nil {
			for c := range changesChan {
				changes = append(changes, c)
			}
		}
		goon.DumpExpr(changes)*/
	}

	if updatesAvailable == 0 {
		io.WriteString(w, `<script>document.getElementById("no_updates").style.display = "";</script>`)
	}

	fmt.Printf("Part 3: %v ms.\n", time.Since(started).Seconds()*1000)
}

// WebSocket handler, to exit when client tab is closed.
func openedHandler(ws *websocket.Conn) {
	// Wait until connection is closed.
	io.Copy(ioutil.Discard, ws)

	//fmt.Println("Exiting, since the client tab was closed (detected closed WebSocket connection).")
	//close(updateRequestChan)
}

// ---

var t *template.Template

func loadTemplates() error {
	var err error
	t = template.New("").Funcs(template.FuncMap{
		"updateSupported": func() bool { return updater != nil },
	})
	t, err = vfstemplate.ParseGlob(assets, t, "/assets/*.tmpl")
	return err
}

var (
	httpFlag     = flag.String("http", "localhost:7043", "Listen for HTTP connections on this address.")
	stdinFlag    = flag.Bool("stdin", false, "Read the list of newline separated Go packages from stdin.")
	godepsFlag   = flag.String("godeps", "", "Read the list of Go packages from the specified Godeps.json file.")
	govendorFlag = flag.String("govendor", "", "Read the list of Go packages from the specified vendor.json file.")
)

func usage() {
	fmt.Fprint(os.Stderr, "Usage: Go-Package-Store [flags]\n")
	fmt.Fprint(os.Stderr, "       [newline separated packages] | Go-Package-Store -stdin [flags]\n")
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `
Examples:
  # Check for updates for all Go packages in GOPATH.
  Go-Package-Store

  # Show updates for all dependencies (recursive) of package in cur working dir.
  go list -f '{{join .Deps "\n"}}' . | Go-Package-Store -stdin

  # Show updates for all dependencies listed in vendor.json file.
  Go-Package-Store -govendor /path/to/vendor.json
`)
}

func main() {
	flag.Usage = usage
	flag.Parse()

	switch {
	default:
		fmt.Println("Using all Go packages in GOPATH.")
		goPackages = &exp14.GoPackages{SkipGoroot: true} // All Go packages in GOPATH (not including GOROOT).
		updater = repo.GopathUpdater{GoPackages: goPackages}
	case *stdinFlag:
		fmt.Println("Reading the list of newline separated Go packages from stdin.")
		goPackages = &exp14.GoPackagesFromReader{Reader: os.Stdin}
		updater = repo.GopathUpdater{GoPackages: goPackages}
	case *godepsFlag != "":
		fmt.Println("Reading the list of Go packages from Godeps.json file:", *godepsFlag)
		goPackages = newGoPackagesFromGodeps(*godepsFlag)
		loadGoPackagesFromGodeps(*godepsFlag, universe)
		updater = nil
	case *govendorFlag != "":
		fmt.Println("Reading the list of Go packages from vendor.json file:", *govendorFlag)
		goPackages = newGoPackagesFromGovendor(*govendorFlag)
		updater = nil
	}

	err := loadTemplates()
	if err != nil {
		log.Fatalln("loadTemplates:", err)
	}

	http.HandleFunc("/index.html", mainHandler)
	http.Handle("/favicon.ico", http.NotFoundHandler())
	http.Handle("/assets/", gzip_file_server.New(assets))
	http.Handle("/opened", websocket.Handler(openedHandler)) // Exit server when client tab is closed.
	if updater != nil {
		http.HandleFunc("/-/update", updateHandler)
		go updateWorker()
	}

	// Start listening first.
	listener, err := net.Listen("tcp", *httpFlag)
	if err != nil {
		log.Fatalf("failed to listen on %q: %v\n", *httpFlag, err)
	}

	switch production {
	case true:
		// Open a browser tab and navigate to the main page.
		go u4.Open("http://" + *httpFlag + "/index.html")
	case false:
		updater = repo.MockUpdater{}
	}

	fmt.Println("Go Package Store server is running at http://" + *httpFlag + "/index.html.")

	err = http.Serve(listener, nil)
	if err != nil {
		log.Fatalln(err)
	}
}
