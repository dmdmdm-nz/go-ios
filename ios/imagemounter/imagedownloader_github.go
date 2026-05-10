package imagemounter

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	githubOwner     = "doronz88"
	githubRepo      = "DeveloperDiskImage"
	githubBranch    = "main"
	personalizedDDI = "PersonalizedImages/Xcode_iOS_DDI_Personalized"
	latestDDI       = "17E5179g"
)

var githubHTTPClient = &http.Client{Timeout: 2 * time.Minute}

func downloadPersonalizedDDI(baseDir string) (string, error) {
	cacheDir := filepath.Join(baseDir, "ddi-latest")
	restoreDir := filepath.Join(cacheDir, "Restore")
	manifestPath := filepath.Join(restoreDir, "BuildManifest.plist")

	if cached, err := isDDICacheValid(manifestPath); err == nil && cached {
		log.Infof("using already downloaded DDI (build %s) from: %s", latestDDI, restoreDir)
		return restoreDir, nil
	}

	log.Infof("downloading personalized DDI (build %s) from GitHub", latestDDI)

	if err := os.MkdirAll(restoreDir, 0o755); err != nil {
		return "", err
	}

	manifestBytes, err := downloadGitHubFile(personalizedDDI + "/BuildManifest.plist")
	if err != nil {
		return "", fmt.Errorf("downloadPersonalizedDDI: failed to download BuildManifest.plist: %w", err)
	}
	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		return "", fmt.Errorf("downloadPersonalizedDDI: failed to write BuildManifest.plist: %w", err)
	}

	manifest, err := loadBuildManifest(manifestPath)
	if err != nil {
		return "", fmt.Errorf("downloadPersonalizedDDI: failed to parse BuildManifest.plist: %w", err)
	}

	dmgPath, trustPath := getFilePaths(manifest)
	if dmgPath == "" || trustPath == "" {
		return "", fmt.Errorf("downloadPersonalizedDDI: could not determine DMG/trustcache paths from manifest")
	}

	dmgFullPath := filepath.Join(restoreDir, dmgPath)
	trustFullPath := filepath.Join(restoreDir, trustPath)

	if err := downloadGitHubFileToDisk(personalizedDDI+"/Image.dmg", dmgFullPath); err != nil {
		return "", fmt.Errorf("downloadPersonalizedDDI: failed to download Image.dmg: %w", err)
	}
	if err := downloadGitHubFileToDisk(personalizedDDI+"/Image.dmg.trustcache", trustFullPath); err != nil {
		return "", fmt.Errorf("downloadPersonalizedDDI: failed to download Image.dmg.trustcache: %w", err)
	}

	log.Infof("successfully downloaded DDI to: %s", restoreDir)
	return restoreDir, nil
}

func getFilePaths(manifest buildManifest) (dmgPath string, trustPath string) {
	if len(manifest.BuildIdentities) == 0 {
		return "", ""
	}
	identity := manifest.BuildIdentities[0]
	for _, i := range manifest.BuildIdentities {
		if i.dmgPath() != "" {
			identity = i
			break
		}
	}
	return identity.dmgPath(), identity.trustCachePath()
}

func isDDICacheValid(manifestPath string) (bool, error) {
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		return false, nil
	}
	manifest, err := loadBuildManifest(manifestPath)
	if err != nil {
		return false, nil
	}
	if manifest.ProductBuildVersion != latestDDI {
		log.Infof("cached DDI build is %q, need %q — will re-download",
			manifest.ProductBuildVersion, latestDDI)
		return false, nil
	}
	dmgPath, trustPath := getFilePaths(manifest)
	if dmgPath == "" || trustPath == "" {
		return false, nil
	}
	restoreDir := filepath.Dir(manifestPath)
	if _, err := os.Stat(filepath.Join(restoreDir, dmgPath)); os.IsNotExist(err) {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(restoreDir, trustPath)); os.IsNotExist(err) {
		return false, nil
	}
	return true, nil
}

func downloadGitHubFile(path string) ([]byte, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
		githubOwner, githubRepo, githubBranch, path)
	resp, err := githubHTTPClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("downloadGitHubFile: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloadGitHubFile: unexpected status %d for %s", resp.StatusCode, url)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("downloadGitHubFile: failed to read response: %w", err)
	}
	return data, nil
}

func downloadGitHubFileToDisk(path string, destPath string) error {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
		githubOwner, githubRepo, githubBranch, path)
	resp, err := githubHTTPClient.Get(url)
	if err != nil {
		return fmt.Errorf("downloadGitHubFileToDisk: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloadGitHubFileToDisk: unexpected status %d for %s", resp.StatusCode, url)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("downloadGitHubFileToDisk: failed to create file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("downloadGitHubFileToDisk: failed to write file: %w", err)
	}
	return nil
}
