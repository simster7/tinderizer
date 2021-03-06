package extractor

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/darkhelmet/env"
	"github.com/darkhelmet/mercury"
	"github.com/darkhelmet/tinderizer/boots"
	"github.com/darkhelmet/tinderizer/hashie"
	J "github.com/darkhelmet/tinderizer/job"
	"golang.org/x/net/html"
)

const (
	FriendlyMessage = "Sorry, extraction failed."
	RetryTimes      = 3
	RetryPause      = 3 * time.Second
)

type JSON map[string]interface{}

var (
	timeout = 5 * time.Second
	token   = env.String("MERCURY_TOKEN")
	logger  = log.New(os.Stdout, "[extractor] ", env.IntDefault("LOG_FLAGS", log.LstdFlags|log.Lmicroseconds))
	merc    = mercury.New(token, logger)
)

type Extractor struct {
	merc   *mercury.Endpoint
	wg     sync.WaitGroup
	Input  <-chan J.Job
	Output chan<- J.Job
	Error  chan<- J.Job
}

func New(merc *mercury.Endpoint, input <-chan J.Job, output chan<- J.Job, error chan<- J.Job) *Extractor {
	return &Extractor{
		merc:   merc,
		Input:  input,
		Output: output,
		Error:  error,
	}
}

func (e *Extractor) error(job J.Job, format string, args ...interface{}) {
	logger.Printf(format, args...)
	job.Friendly = FriendlyMessage
	e.Error <- job
}

func (e *Extractor) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	for job := range e.Input {
		e.wg.Add(1)
		go e.Process(job)
	}
	e.wg.Wait()
	close(e.Output)
}

func (e *Extractor) extract(url string) (*mercury.Response, error) {
	return merc.Extract(url)
}

func (e *Extractor) Process(job J.Job) {
	job.Progress("Extracting...")

	defer e.wg.Done()
	resp, err := e.extract(job.Url)
	if err != nil {
		e.error(job, "%s", err)
		return
	}

	doc, err := rewriteAndDownloadImages(job.Root(), resp.Content)
	if err != nil {
		e.error(job, "HTML parsing failed: %s", err)
		return
	}

	job.Doc = doc
	if resp.Title != "" {
		job.Title = resp.Title
	}
	job.Domain = resp.Domain

	job.Progress("Extraction complete...")
	e.Output <- job
}

func cleanSrcset(val string) string {
	re := regexp.MustCompile(`(?:(?P<url>[^"'\s,]+)\s*(?:\s+\d+[wx])(?:,\s*)?)`)
	match := re.FindStringSubmatch(val)
	if match == nil {
		return val
	}
	return match[1]
}

func srcset(node *html.Node) bool {
	return attrIndex(node, "srcset") > -1
}

func attrIndex(node *html.Node, key string) int {
	for index, attr := range node.Attr {
		if attr.Key == key {
			return index
		}
	}
	return -1
}

func rewriteAndDownloadImages(root string, content string) (*html.Node, error) {
	var wg sync.WaitGroup
	imageDownloader := newDownloader(root, timeout)
	doc, err := boots.Walk(strings.NewReader(content), "img", func(node *html.Node) {
		var index int
		var attr html.Attribute
		var uri string
		if srcset(node) {
			index = attrIndex(node, "srcset")
			if index < 0 {
				return
			}
			attr = node.Attr[index]
			uri = cleanSrcset(attr.Val)
		} else {
			index = attrIndex(node, "src")
			if index < 0 {
				return
			}
			attr = node.Attr[index]
			uri = attr.Val
		}
		altered := fmt.Sprintf("%x.jpg", hashie.Sha1([]byte(uri)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Printf("downloading image: %s", uri)
			if err := imageDownloader.downloadToFile(uri, altered); err != nil {
				logger.Printf("downloading image failed: %s", err)
			}
		}()
		node.Attr[attrIndex(node, "src")].Val = altered
	})
	wg.Wait()
	logger.Println("finished rewriting images")
	return doc, err
}
