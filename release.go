package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	logrus "github.com/Sirupsen/logrus"
	"github.com/hashicorp/go-retryablehttp"
)

var cveRegex = regexp.MustCompile(`CVE\-[0-9]{4}\-[0-9]{4,}`)

const (
	cveUri      string = "http://cve.circl.lu/api/cve/%s"
	releasesUri string = "https://coreos.com/releases/releases.json"
)

type cve struct {
	ID   string  `json:"id"`
	CVSS float64 `json:"cvss"`
	err  error
}

type coreOSRelease struct {
	SecurityFixes []cve      `json:"securityFixes,omitempty"`
	Version       string     `json:"version"`
	ReleaseNotes  string     `json:"releaseNotes"`
	MaxCVSS       *float64   `json:"maxCvss,omitempty"`
	ReleasedOn    *time.Time `json:"releasedOn,omitempty"`
}

type releaseRepository struct {
	sync.RWMutex
	client           *retryablehttp.Client
	channel          string
	installedVersion coreOSRelease
	latestVersion    coreOSRelease
	err              error
	releaseConfPath  string
	updateConfPath   string
}

func newReleaseRepository(client *http.Client, releaseConfPath string, updateConfPath string) *releaseRepository {
	logWriter := logrus.StandardLogger().Writer()
	retryableClient := &retryablehttp.Client{
		HTTPClient:   client,
		Logger:       log.New(logWriter, "", log.LstdFlags),
		RetryWaitMin: 100 * time.Millisecond,
		RetryWaitMax: 2 * time.Second,
		RetryMax:     5,
		CheckRetry:   retryablehttp.DefaultRetryPolicy,
		Backoff:      retryablehttp.DefaultBackoff,
	}
	return &releaseRepository{
		client:          retryableClient,
		releaseConfPath: releaseConfPath,
		updateConfPath:  updateConfPath,
	}
}

func (r *releaseRepository) UpdateError(err error) {
	r.Lock()
	defer r.Unlock()
	r.err = err
}

func (r *releaseRepository) GetChannel() error {
	channel, err := valueFromFile("GROUP=", r.updateConfPath)
	if err != nil {
		return err
	}

	r.Lock()
	defer r.Unlock()
	// in K8S we use CoreUpdate, which uses a non-standard channel, like "coreUpdateChan1"
	// if we encounter a non-standard channel, we default the channel to "stable"
	if channel == "beta" || channel == "alpha" {
		r.channel = channel
	} else {
		r.channel = "stable"
	}
	return nil
}

func (r *releaseRepository) GetInstalledVersion() error {
	release, err := valueFromFile("COREOS_RELEASE_VERSION=", r.releaseConfPath)
	if err != nil {
		return err
	}

	enrichedRelease, err := r.Get(release)
	if err != nil {
		return err
	}

	r.Lock()
	defer r.Unlock()

	r.installedVersion = *enrichedRelease
	return nil
}

func (r *releaseRepository) GetLatestVersion() error {
	uri := fmt.Sprintf(versionUri, r.channel)
	req, err := retryablehttp.NewRequest("GET", uri, nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("Got %v requesting %v", resp.StatusCode, uri)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	release, err := parseCoreOSVersion(string(body))
	if err != nil {
		return err
	}

	coreOS, err := r.Get(release)
	if err != nil {
		return err
	}

	r.Lock()
	defer r.Unlock()

	r.latestVersion = *coreOS
	return nil
}

func parseCoreOSVersion(body string) (string, error) {
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "COREOS_VERSION=") {
			return strings.TrimPrefix(line, "COREOS_VERSION="), nil
		}
	}

	return "", errors.New("No CoreOS version on the page")
}

func (r *releaseRepository) Get(release string) (*coreOSRelease, error) {
	releases, err := GetJSON(r.client, releasesUri)
	if err != nil {
		return nil, err
	}

	releaseData, ok := releases[release].(map[string]interface{})
	if !ok {
		return nil, errors.New("Release not found")
	}

	releaseNotes := releaseData["release_notes"].(string)
	releasedOnText, ok := releaseData["release_date"]

	var releasedOn *time.Time
	if ok {
		parsed, err := time.Parse("2006-01-02 15:04:05 -0700", releasedOnText.(string))
		if err == nil {
			releasedOn = &parsed
		}
	}

	cveIDs := parseReleaseNotes(releaseNotes)
	var securityFixes []cve
	var maxCVSS float64 = -1

	for _, cveID := range cveIDs {
		fix := r.retrieveCVE(cveID)
		securityFixes = append(securityFixes, fix)
		maxCVSS = math.Max(maxCVSS, fix.CVSS)
	}

	return &coreOSRelease{ReleasedOn: releasedOn, ReleaseNotes: releaseNotes, SecurityFixes: securityFixes, MaxCVSS: &maxCVSS, Version: release}, nil
}

func parseReleaseNotes(notes string) []string {
	cveIDs := cveRegex.FindAllString(notes, -1)
	if len(cveIDs) == 0 {
		return cveIDs
	}

	uniqueCVEs := make(map[string]struct{})
	for _, cveID := range cveIDs {
		uniqueCVEs[cveID] = struct{}{}
	}

	result := make([]string, 0)
	for cveID := range uniqueCVEs {
		result = append(result, cveID)
	}
	return result
}

func (r *releaseRepository) retrieveCVE(id string) cve {
	cveResult, err := GetJSON(r.client, fmt.Sprintf(cveUri, id))
	if err != nil {
		return cve{err: err, ID: id}
	}

	cvssString, ok := cveResult["cvss"].(string)
	if !ok {
		return cve{err: errors.New("No CVSS found!"), ID: id}
	}
	cvss, err := strconv.ParseFloat(cvssString, 64)
	if err != nil {
		return cve{
			err: errors.New(fmt.Sprintf("Cannot parse CVSS %s because %v", cvssString, err.Error())),
			ID:  id,
		}
	}

	return cve{CVSS: cvss, ID: id, err: nil}
}
