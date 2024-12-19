package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"log/slog"
	"log/syslog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/spf13/viper"
	"github.com/tidwall/buntdb"
)

var defaultBase = "http://127.0.0.1:8080/s3"
var db *buntdb.DB
var client http.Client
var connSemaphore *semaphore.Weighted
var defaultKey string

// errorPage logs the issue and sends an error page to the user with
// the given status code and message
func errorPage(w http.ResponseWriter, code int, message string) {
	slog.Error("Returning error page status", "code",
		code, "message", message)

	http.Error(w, "An error occurred and then there were more", http.StatusInternalServerError)
	return
}

func logAccess(req *http.Request) {
	slog.Info("Handling request", "remote", req.RemoteAddr, "method", req.Method, "URL", req.URL)
}

func releaseSemaphore() {
	// Handle odd state of sometimes releasing more than held,
	// that implies something serious but we don't care
	// as we only use this as a simple limit
	defer func() { recover() }()
	connSemaphore.Release(1)
}

// doRequest is what does actual proxying to the backend
func doRequest(req *http.Request) (*http.Response, error) {

	connSemaphore.Acquire(req.Context(), 1)
	// release the semaphore when we're done here (note that it doesn't mean
	// the outgoing connection is done, only limit what we do here)
	defer releaseSemaphore()

	baseURL := viper.GetString("baseURL")

	realURL, _ := url.JoinPath(baseURL, req.URL.Path)

	newURL, err := url.Parse(realURL)
	if err != nil {
		return nil, fmt.Errorf("Error from parsing constructed proxied url: %v", err)
	}

	outReq, err := http.NewRequestWithContext(req.Context(), req.Method, newURL.String(), nil)

	if err != nil {
		return nil, fmt.Errorf("Error while creating request: %v", err)
	}

	for k := range (*req).Header {
		outReq.Header.Add(k, req.Header.Get(k))
	}

	_, already := outReq.Header["Client-Public-Key"]
	if !already {
		outReq.Header.Add("Client-Public-Key", defaultKey)
	}

	slog.Debug("Connecting to backend", "url", realURL, "remote", req.RemoteAddr)
	return client.Do(outReq)
}

func listBuckets(w http.ResponseWriter, req *http.Request) {
	logAccess(req)
	slog.Debug("listbuckets called")
	// List Buckets, we don't bother with cache here

	if req == nil || req.URL == nil || req.URL.Query() == nil {
		slog.Warn("Bad request", "remote", req.RemoteAddr)
		errorPage(w, http.StatusInternalServerError, "Something is seriously wrong")
		return
	}

	r, err := doRequest(req)

	if err != nil {
		slog.Warn("Error while proxying request for listObject", "err", err)
		errorPage(w, http.StatusInternalServerError, "Error while proxying request")
		return
	}

	defer r.Body.Close()

	if r.StatusCode != http.StatusOK {
		// Not success - copy response
		copyHeadersOut(r, w)
		w.WriteHeader(r.StatusCode)
		io.Copy(w, r.Body)
		return
	}

	copyHeadersOut(r, w)

	w.WriteHeader(http.StatusOK)

	io.Copy(w, r.Body)

	slog.Debug("Listbuckets done", "remote", req.RemoteAddr)
}

type Owner struct {
	DisplayName string `xml:"DisplayName,omitempty"`
	ID          string `xml:"ID,omitempty"`
}

type Object struct {
	ChecksumAlgorithm []string `xml:"ChecksumAlgorithm,omitempty"`
	ETag              string   `xml:"ETag,omitempty"`
	Key               string   `xml:"Key"`
	Owner             Owner    `xml:"Owner,omitempty"`
	LastModified      string   `xml:"LastModified,omitempty"`
	Size              int      `xml:"Size,omitempty"`
	StorageClass      string   `xml:"StorageClass,omitempty"`
}

type ListBucketResult struct {
	XMLName        xml.Name `xml:"ListBucketResult"`
	xmlns          string   `xml:"xmlns,attr"`
	CommonPrefixes []string `xml:"CommonPrefixes>Prefix"`
	Contents       []Object `xml:"Contents"`
	Delimiter      string   `xml:"Delimiter,omitempty"`
	EncodingType   string   `xml:"EncodingType,omitempty"`
	IsTruncated    bool     `xml:"IsTruncated"`
	Marker         string   `xml:"Marker,omitempty"`
	MaxKeys        int      `xml:"MaxKeys,omitempty"`
	KeyCount       int      `xml:"KeyCount"`
	Name           string   `xml:"Name"`
	NextMarker     string   `xml:"NextMarker,omitempty"`
	Prefix         string   `xml:"Prefix"`
}

func cacheSet(objectPath, value string) error {
	// fmt.Printf("Setting object for %v to %v\n\n", objectPath, object)
	// objectText, err := xml.Marshal(object)
	// fmt.Printf("Setting object for %v to %v\n\n", objectPath, object)

	err := db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(objectPath, value, &buntdb.SetOptions{Expires: true, TTL: 100 * time.Minute})
		return err
	})

	// fmt.Printf("Setting returned object  %v for %v\n\n", err, objectPath)

	// k, l := cacheGet(objectPath)
	// fmt.Printf("Reading after set %v  %v\n\n", k, l)

	return err
}

// cacheKey generates the appropriate key for cache use
// we include the authorization
func cacheKey(path string, req *http.Request) string {
	t := req.Header.Get("x-amz-security-token")

	if t != "" {
		return fmt.Sprintf("%s,%s", path, string(t))
	}

	r := req.Header.Get("Authorization")

	return fmt.Sprintf("%s,%s", path, string(r))
}

func cacheGet(objectPath string) (string, error) {

	// fmt.Printf("Getting object from cache %v\n\n", objectPath)

	var object string

	err := db.View(func(tx *buntdb.Tx) error {
		val, err := tx.Get(objectPath)
		if err != nil {
			return err
		}
		object = val
		return nil
	})

	//	fmt.Printf("object is %v\n\n", object)

	return object, err
}

func copyHeadersOut(r *http.Response, w http.ResponseWriter) {
	for k := range r.Header {
		if k != "Content-Length" {
			w.Header().Add(k, r.Header.Get(k))
		}
	}

}

func getObjectInKeyPrefix(k, delim string) func(Object) bool {
	checkFor := k + delim
	return func(o Object) bool {
		// fmt.Printf("Checking for %v in %v", checkFor, o.Key)
		return strings.Index(o.Key, checkFor) == 0
	}

}

func fixTimeStamp(o *Object) {
	if o.LastModified != "" {
		timestamp, err := time.Parse(time.RFC1123, o.LastModified)

		if err == nil {
			// Just ignore any error
			o.LastModified = timestamp.Format(time.RFC3339)
		}
	}
}

func fillObject(o *Object, req *http.Request) {

	// No fill in needed

	slog.Debug("Filling object", "object", o, "remote", req.RemoteAddr)

	// Fix timestamp if needed
	fixTimeStamp(o)

	if o.ETag != "" && o.LastModified != "" {
		slog.Debug("No need to fill more", "object", o, "remote", req.RemoteAddr)

		return
	}

	objectPath := fmt.Sprintf("%s/%s", req.PathValue("bucket"), o.Key)
	lookupKey := cacheKey(objectPath, req)

	cache, err := cacheGet(lookupKey)
	// fmt.Printf("from cache %v  %v\n", objectPath, err)

	slog.Debug("Result of object cache lookup", "cache", cache, err, "err", "lookupKey", lookupKey, "remote", req.RemoteAddr)

	var cacheO Object
	if err == nil {
		err = xml.Unmarshal([]byte(cache), &cacheO)
	}

	if cacheO.ETag != "" && cacheO.LastModified != "" {
		(*o).ETag = cacheO.ETag
		(*o).LastModified = cacheO.LastModified
		slog.Debug("Filled from cache", "object", o, "remote", req.RemoteAddr)
		return
	}

	// fmt.Printf("Doing web request for %v\n", o)
	r := http.Request{Method: "HEAD"}
	r.Header = req.Header
	r.RemoteAddr = req.RemoteAddr // Pass through remote address for logging

	objURL, _ := url.JoinPath(req.URL.Path, o.Key)
	r.URL = &url.URL{Path: objURL}

	slog.Debug("Doing backend request", "object", o, "remote", req.RemoteAddr, "url", r.URL)

	resp, err := doRequest(&r)

	if err != nil {
		slog.Debug("Connection to fill in needed details failed",
			"object", objectPath, "err", err, "remote", req.RemoteAddr)
		// Error but we ignore it
		return
	}

	if resp.StatusCode != http.StatusOK {
		slog.Debug("Connection to fill in needed details failed",
			"object", objectPath, "err", err,
			"status", resp.StatusCode,
			"remote", req.RemoteAddr)

		return
	}

	lastModified := resp.Header.Get("Last-Modified")
	etag := resp.Header.Get("ETag")

	if o.ETag == "" {
		o.ETag = etag
	}

	if o.LastModified == "" {
		o.LastModified = lastModified
	}

	fixTimeStamp(o)

	if cacheO.Size != 0 && o.Size == 0 {
		o.Size = cacheO.Size
	}

	slog.Debug("Putting into cache", "object", o, "remote", req.RemoteAddr, "lookupKey", lookupKey)

	//	fmt.Printf("Storing object in cache after lookup %v\n", objectPath)
	// Store in cache
	v, err := xml.Marshal(*o)
	if err != nil {
		slog.Debug("Cache marshal result", "object", o, "remote", req.RemoteAddr, "lookupKey", lookupKey, "err", err)
	}
	if err == nil {
		err = cacheSet(lookupKey, string(v))
		slog.Debug("Cache update result", "object", o, "remote", req.RemoteAddr, "lookupKey", lookupKey, "err", err)
	}

	_, _ = ioutil.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return
}

type bucketListCache struct {
	Headers map[string][]string `json:"headers"`
	Content []byte              `json:"content"`
}

func listObjects(w http.ResponseWriter, req *http.Request) {
	logAccess(req)
	slog.Debug("listObjects started", "bucket", req.PathValue("bucket"),
		"remote", req.RemoteAddr)

	if req == nil || req.URL == nil || req.URL.Query() == nil {
		slog.Warn("Bad request", "remote", req.RemoteAddr)
		errorPage(w, http.StatusInternalServerError, "Something is seriously wrong")
		return
	}

	var buf []byte

	bucketListPath := req.PathValue("bucket")
	lookupKey := cacheKey(bucketListPath, req)
	pageCache, err := cacheGet(lookupKey)

	if err == nil {
		c := bucketListCache{}

		err = json.Unmarshal([]byte(pageCache), &c)

		if err == nil {
			// No error here

			for k, v := range c.Headers {
				if k != "Content-Length" {
					w.Header().Add(k, v[0])
				}
			}

			if req.Method == "HEAD" {
				slog.Debug("listObjects answering HEAD request from cache", "bucket", req.PathValue("bucket"),
					"remote", req.RemoteAddr)
				w.WriteHeader(http.StatusOK)
				return
			}

			buf = c.Content
			slog.Debug("listObjects got contents from cache", "bucket", req.PathValue("bucket"),
				"remote", req.RemoteAddr)

		}
	}

	// Get from cache failed
	if len(buf) == 0 {
		slog.Debug("listObjects proxying request", "bucket", req.PathValue("bucket"),
			"remote", req.RemoteAddr)

		r, err := doRequest(req)
		if err != nil {
			slog.Warn("Error while proxying request for listObject", "err", err,
				"bucket", req.PathValue("bucket"), "remote", req.RemoteAddr)
			errorPage(w, http.StatusInternalServerError, "Error while proxying request")
			return
		}
		defer r.Body.Close()

		if r.StatusCode != http.StatusOK {
			// Not success - copy response
			slog.Info("Proxied request for listObject gave non-ok status", "err", err,
				"bucket", req.PathValue("bucket"), "remote", req.RemoteAddr,
				"status", r.StatusCode)

			copyHeadersOut(r, w)
			w.WriteHeader(r.StatusCode)
			io.Copy(w, r.Body)
			return
		}

		copyHeadersOut(r, w)

		buf, err = io.ReadAll(r.Body)
		if err != nil {
			slog.Warn("Error while reading response for listObject", "err", err,
				"bucket", req.PathValue("bucket"), "remote", req.RemoteAddr)
			errorPage(w, http.StatusInternalServerError, "Error while proxying request")
			return
		}

		// Got the page, now let's cache it for future use
		c := bucketListCache{Headers: req.Header, Content: buf}

		cacheObj, err := json.Marshal(c)

		if err == nil {
			err = cacheSet(lookupKey, string(cacheObj))
			slog.Debug("cache update failed after fetch", "bucket", req.PathValue("bucket"), "lookupKey", lookupKey,
				"remote", req.RemoteAddr)

		}

		if req.Method == "HEAD" {
			slog.Debug("listObjects answering HEAD request after request", "bucket", req.PathValue("bucket"),
				"remote", req.RemoteAddr)
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	req.ParseForm()
	prefix, prefixExists := req.URL.Query()["prefix"]
	delim, delimExists := req.URL.Query()["delimiter"]

	// If delimiter is set but empty, use /
	if delimExists && delim[0] == "" {
		delim = []string{"/"}
	}

	x := ListBucketResult{xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}

	if err := xml.Unmarshal(buf, &x); err != nil {
		slog.Warn("Error while reading response for listObject", "err", err,
			"bucket", req.PathValue("bucket"), "remote", req.RemoteAddr)

		errorPage(w, http.StatusInternalServerError, "Error while proxying request")
		return
	}

	x.xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

	if prefixExists {

		switch len(prefix[0]) {
		case 0:
			// Prefix specified but empty, set prefix in response,
			// and mountpoint doesn't like it empty
			x.Prefix = ""
			// no filtering done here
		default:
			x.Prefix = prefix[0]

			// Make a new list and move the objects that match
			newContents := make([]Object, 0)
			for _, c := range x.Contents {

				if len(x.Prefix) <= len(c.Key) && c.Key[:len(x.Prefix)] == x.Prefix {
					newContents = append(newContents, c)
				}
			}

			x.Contents = newContents
		}
	}

	// Keys are filtered to prefix here
	if delimExists {
		commonPrefixes := make([]string, 0)
		commonFilteredContents := make([]Object, 0)

		// if x.Prefix != "" {
		// 	addFakeObject(req, x.Prefix)
		// 	addFakeObject(req, strings.TrimSuffix(x.Prefix, "/"))
		// }

		for _, c := range x.Contents {
			considerKey := c.Key
			if x.Prefix != "" {
				considerKey = considerKey[len(x.Prefix):]
			}

			keyPrefix, _, found := strings.Cut(considerKey, delim[0])

			usePrefix := fmt.Sprintf("%s%s%s", x.Prefix, keyPrefix, delim[0])
			if found {
				// prefix = c.Key[:]
				if !slices.Contains(commonPrefixes, usePrefix) {

					// addFakeObject(req, usePrefix)
					// addFakeObject(req, strings.TrimSuffix(usePrefix, "/"))

					commonPrefixes = append(commonPrefixes, usePrefix)
				}
			} else {
				commonFilteredContents = append(commonFilteredContents, c)
			}
		}

		x.CommonPrefixes = commonPrefixes
		x.Contents = commonFilteredContents
	}

	// Walk through contents and set needed bits if missing

	wg := sync.WaitGroup{}
	for i := range x.Contents {
		wg.Add(1)

		go func() {
			defer wg.Done()
			fillObject(&x.Contents[i], req)
		}()

	}

	wg.Wait()
	x.KeyCount = len(x.Contents)

	objectListResponse, err := xml.MarshalIndent(x, "  ", "    ")

	// if !bytes.Contains(objectListResponse, []byte("<Contents>")) {
	// 	// FIXME: Can we not get XML to encode the empty slice?
	// 	i := bytes.Index(objectListResponse, []byte("<IsTruncated>"))

	// 	response := make([]byte, 0)
	// 	response = append(response, objectListResponse[:i]...)
	// 	response = append(response, []byte("<Contents></Contents>")...)
	// 	response = append(response, objectListResponse[i:]...)

	// 	objectListResponse = response
	// }

	if bytes.Contains(objectListResponse, []byte("<CommonPrefixes></CommonPrefixes>")) {
		i := bytes.Index(objectListResponse, []byte("<CommonPrefixes></CommonPrefixes>"))

		response := make([]byte, 0)
		response = append(response, objectListResponse[:i]...)

		response = append(response, objectListResponse[i+33:]...)

		objectListResponse = response
	}

	if bytes.Contains(objectListResponse, []byte("<ListBucketResult>")) {
		// 	// FIXME: Can we not get XML to encode the empty slice?
		i := bytes.Index(objectListResponse, []byte("<ListBucketResult>"))

		response := make([]byte, 0)
		response = append(response, objectListResponse[:i+17]...)
		response = append(response, []byte(" xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\"")...)
		response = append(response, objectListResponse[i+17:]...)

		objectListResponse = response
	}

	w.WriteHeader(http.StatusOK)
	w.Write(objectListResponse)

	slog.Debug("listObjects done", "bucket", req.PathValue("bucket"),
		"remote", req.RemoteAddr)

	return

}

func addFakeObject(req *http.Request, prefix string) {

	objectPath := fmt.Sprintf("%/%", req.PathValue("bucket"), prefix)
	fakeObject := Object{Key: prefix}
	lookupKey := cacheKey(objectPath, req)

	v, err := xml.Marshal(fakeObject)
	if err != nil {
		slog.Debug("Failed to marshall fake object", "object", fakeObject, "prefix", prefix, "remote", req.RemoteAddr, "lookupKey", lookupKey, "err", err)
	}
	if err == nil {
		err = cacheSet(lookupKey, string(v))
		// slog.Debug("Cache update result", "object", fakeObject, "remote", req.RemoteAddr, "lookupKey", lookupKey, "err", err)
	}

}

func getObject(w http.ResponseWriter, req *http.Request) {
	logAccess(req)

	slog.Debug("getObject", "remote", req.RemoteAddr, "bucket",
		req.PathValue("bucket"), "object", req.PathValue("object"))

	if req == nil || req.URL == nil || req.URL.Query() == nil {
		slog.Warn("Bad request", "remote", req.RemoteAddr)
		errorPage(w, http.StatusInternalServerError, "Something is seriously wrong")
		return
	}

	objectPath := fmt.Sprintf("%/%", req.PathValue("bucket"), req.PathValue("object"))
	lookupKey := cacheKey(objectPath, req)
	cache, err := cacheGet(lookupKey)
	var cacheObject Object

	if err == nil {
		err = xml.Unmarshal([]byte(cache), &cacheObject)
		if err != nil {
			slog.Debug("Unmarshal from cache failed in getObject", "err", err, "remote", req.RemoteAddr)
		}
	}

	if req.Method == "HEAD" && cacheObject.Key != "" {
		slog.Debug("Satisfying HEAD request from cache with 200 for getObject",
			"err", err, "bucket", req.PathValue("bucket"),
			"object", req.PathValue("object"), "remote", req.RemoteAddr)

		// Satisfy from cache
		w.WriteHeader(http.StatusOK)
		return
	}

	r, err := doRequest(req)

	if err != nil {
		slog.Warn("Error while proxying request for getObject", "err", err, "remote", req.RemoteAddr)
		errorPage(w, http.StatusInternalServerError, "Error while proxying request")
		return
	}

	defer r.Body.Close()

	if r.StatusCode != http.StatusOK {
		// Not success - copy response
		slog.Info("Response from backend was not ok", "status", r.StatusCode, "remote", req.RemoteAddr)

		copyHeadersOut(r, w)
		w.WriteHeader(r.StatusCode)
		io.Copy(w, r.Body)
		return
	}

	if cacheObject.Size != 0 {
		if req.Header.Get("Range") != "" {
			requestedRange := strings.Replace(req.Header.Get("Range"), "=", " ", 2)
			w.Header().Add("Content-Range", fmt.Sprintf("%s/%d", requestedRange, cacheObject.Size))
		} else {
			w.Header().Add("Content-Range", fmt.Sprintf("bytes 0-%d/%d", cacheObject.Size-1, cacheObject.Size))
		}
	}
	copyHeadersOut(r, w)

	if cacheObject.Size != 0 {
		w.Header().Add("Content-Length", fmt.Sprintf("%d", cacheObject.Size))
	}

	w.WriteHeader(http.StatusOK)

	io.Copy(w, r.Body)
	slog.Debug("getObject done", "remote", req.RemoteAddr)
}

func initViper() {
	viper.SetConfigName("bpproxy")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")

	viper.SetEnvPrefix("BPPROXY")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	viper.SetDefault("baseURL", defaultBase)

	// Try to read configuration file and flag if it fails
	// but don't bail out
	err := viper.ReadInConfig()
	if err != nil {
		slog.Error("Error while reading configuration", "error", err)
	}
}

type serverConfig struct {
	listenAddress string
	keyfile       string
	chain         string
}

func makeServerConfig() (serverConfig, error) {
	s := serverConfig{}

	s.listenAddress = viper.GetString("server.listenAddress")
	s.keyfile = viper.GetString("server.keyFile")
	s.chain = viper.GetString("server.chainFile")

	if s.listenAddress == "" {
		s.listenAddress = "127.0.0.1:8090"
	}

	return s, nil
}

func setupLogger() {

	lognameSource := os.Args[0]
	logName := fmt.Sprintf("%s ", lognameSource)

	syslogWriter, err := syslog.New(syslog.LOG_AUTH|syslog.LOG_INFO, logName)
	if err != nil {
		fmt.Printf("Error while setting up syslog logging  error=%v\n", err)
		log.SetOutput(os.Stderr)
	} else {
		myWriter := io.MultiWriter(syslogWriter, os.Stderr)
		log.SetOutput(myWriter)
	}
	slog.SetLogLoggerLevel(slog.LevelDebug)
}

func runInit() {
	setupLogger()
	initViper()

	var err error
	db, err = buntdb.Open(":memory:")

	if err != nil {
		fmt.Printf("Error with database?")
	}

	client = http.Client{Transport: &http.Transport{}}

	connSemaphore = semaphore.NewWeighted(20)

	c4ghKey, ok := viper.Get("defaultkey").(string)
	if ok && c4ghKey != "" {

		_, err = base64.StdEncoding.DecodeString(c4ghKey)
		if err != nil {
			// Supplied base64 encoded key
			defaultKey = c4ghKey
		}

		f, err := os.Open(c4ghKey)
		if err != nil {
			fmt.Printf("Couldn't open requested keyfile %s: %v", c4ghKey, err)
		}
		readKey, err := io.ReadAll(f)

		defaultKey = string(readKey)
		if strings.Contains(defaultKey, "-----BEGIN") {

			defaultKey = base64.StdEncoding.EncodeToString(readKey)
		}

		fmt.Printf("Defautkey is %v\n", defaultKey)
		if err != nil {
			fmt.Printf("Couldn't read requested keyfile %s: %v", c4ghKey, err)
		}
		err = f.Close()
		if err != nil {
			fmt.Printf("Error while closing requested keyfile %s: %v", c4ghKey, err)
		}

	}

}

func doWork() {
	runInit()

	http.HandleFunc("/{bucket}/{object...}", getObject)
	http.HandleFunc("/{bucket}/{$}", listObjects)
	http.HandleFunc("/{bucket}", listObjects)
	http.HandleFunc("/", listBuckets)

	sconfig, err := makeServerConfig()
	if err != nil {
		slog.Error("Error while getting server configuration", "error", err)
		fmt.Printf("Bailing out\n\n")
		os.Exit(1)
	}

	slog.Info("bpproxy is starting", "listenaddress", sconfig.listenAddress)

	httpServer := &http.Server{
		Addr: sconfig.listenAddress,
		//Handler:        myHandler,
		ReadTimeout:    60 * time.Second,
		WriteTimeout:   60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	if sconfig.chain != "" {
		err = httpServer.ListenAndServeTLS(sconfig.chain, sconfig.keyfile)
	} else {
		err = httpServer.ListenAndServe()
	}

	if err != nil {
		slog.Error("Starting listening server failed", "error", err)
		fmt.Printf("Bailing out\n\n")
		os.Exit(1)
	}

}

func main() {

	if len(os.Args) > 1 {
		viper.Set("baseURL", os.Args[1])
	}
	doWork()
}
