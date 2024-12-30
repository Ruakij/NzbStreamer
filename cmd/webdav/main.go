package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"astuart.co/nntp"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/SimpleWebdavFilesystem"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskvGocacheWrapper"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameOps"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/AdaptiveReadaheadCache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/RarFileResource"
	"github.com/eko/gocache/lib/v4/cache"
	"github.com/peterbourgon/diskv/v3"

	"net/http"
	_ "net/http/pprof"
)

const (
	usenetHost    string = ""
	usenetPort    int    = 563
	usenetUser    string = ""
	usenetPass    string = ""
	usenetMaxConn int    = 20
)

var (
	filesystem   *SimpleWebdavFilesystem.FS = SimpleWebdavFilesystem.NewFS()
	segmentCache cache.CacheInterface[[]byte]
)

func main() {
	go func() {
		http.ListenAndServe("localhost:6060", nil)
	}()

	var err error = nil

	// Setup nntpClient
	nntpClient := setupNntpClient(usenetHost, usenetPort, true, usenetUser, usenetPass, usenetMaxConn)

	// Setup cache
	segmentCache, err = diskvGocacheWrapper.NewDiskvCache[[]byte](diskv.Options{
		BasePath: "../../.cache",
	})
	if err != nil {
		panic(err)
	}

	// Load example nzb
	nzbData, err := loadNzbFile("../../.testfiles/test1_mod.nzb")
	if err != nil {
		panic(err)
	}

	// Add as files
	err = createResources(filesystem, nzbData, segmentCache, nntpClient)
	if err != nil {
		panic(err)
	}

	// Serve webdav
	err = setupWebdav(filesystem, ":8080")
	if err != nil {
		panic(err)
	}
}

func loadNzbFile(path string) (*nzbParser.NzbData, error) {
	nzbFile, err := os.OpenFile(path, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("Couldnt read nzbfile: %v\n", err)
	}

	nzbData, err := nzbParser.ParseNzb(nzbFile)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing nzbfile: %v\n", err)
	}

	warnings, errors := nzbData.CheckPlausability()
	if len(warnings) > 0 {
		fmt.Println("Warnings while checking Nzb:")
		for _, warning := range warnings {
			fmt.Println(warning)
		}
	}
	if len(errors) > 0 {
		var errMsg string
		for i, err := range errors {
			if i != len(errors)-1 {
				errMsg += fmt.Sprintf("%v, ", err)
			} else {
				errMsg += fmt.Sprintf("%v", err)
			}
		}
		return nil, fmt.Errorf("errors while checking Nzb: %s", errMsg)
	}
	return nzbData, nil
}

func createResources(filesystem *SimpleWebdavFilesystem.FS, nzbData *nzbParser.NzbData, cache cache.CacheInterface[[]byte], nntpClient *nntp.Client) (err error) {
	namedFileResources := BuildNamedFileResourcesFromNzb(nzbData, cache, nntpClient)

	/*
		for filename, resource := range namedFileResources {
			filesystem.AddFile(fmt.Sprintf("/%s/", nzbData.Meta), filename, resource)
		}
	*/

	// Extract filenames
	filenames := make([]string, 0, len(namedFileResources))
	for filename := range namedFileResources {
		filenames = append(filenames, filename)
	}

	// Group
	groupedFilenames := filenameOps.GroupPartFilenames(filenames)
	//  Sort
	filenameOps.SortGroupedFilenames(groupedFilenames)

	// Add to filesystem
	for groupFilename, filenames := range groupedFilenames {
		if strings.HasSuffix(groupFilename, ".par2") {
			continue
		}

		for _, filename := range filenames {
			resource := namedFileResources[filename]
			// Lowest expected speed					Can help at beginning speeding up
			minCache := 512 * 1024
			// Low-Buffer (When to load more data)		Helps with continous data-flow; too low and flow can stutter; too high and more ressources can be wasted
			lowBuffer := 4 * 1024 * 1024
			// Max expected speed + Low-Buffer			Max speed per iteration
			maxCache := 768000*20 + lowBuffer
			// Add readaheadCache
			readaheadResource := AdaptiveReadaheadCache.NewAdaptiveReadaheadCache(resource, 1*time.Second, 1*time.Second, minCache, maxCache, lowBuffer)
			filesystem.AddFile("/", filename, readaheadResource)
		}

		// Special extensions
		if strings.HasSuffix(groupFilename, ".rar") {
			resources := make([]resource.ReadSeekCloseableResource, len(filenames))
			for i, filename := range filenames {
				resources[i] = namedFileResources[filename]
			}

			resource := RarFileResource.NewRarFileResource(resources, nzbData.Meta[nzbParser.MetaKeyPassword], "")
			files, err := resource.GetRarFiles(1)
			if err != nil {
				return err
			}

			for _, file := range files {
				resource := RarFileResource.NewRarFileResource(resources, nzbData.Meta[nzbParser.MetaKeyPassword], file)
				// Lowest expected speed					Can help at beginning speeding up
				minCache := 512 * 1024
				// Low-Buffer (When to load more data)		Helps with continous data-flow
				lowBuffer := 4 * 1024 * 1024
				// Max expected speed + Low-Buffer			Max speed per iteration
				maxCache := 768000*20 + lowBuffer

				// Add readaheadCache
				readaheadResource := AdaptiveReadaheadCache.NewAdaptiveReadaheadCache(resource, 1*time.Second, 1*time.Second, minCache, maxCache, lowBuffer)
				filesystem.AddFile("/", "readaheadResource-"+file, readaheadResource)
				filesystem.AddFile("/", file, resource)
			}
		}
	}

	return
}
