package phpfpm

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

var (
	fpmConfigCache     = make(map[string]*EffectiveConfig)
	fpmConfigCacheLock sync.Mutex
)

// InvalidateConfigCache forgets the parsed configuration.
//
// Call it after reloading php-fpm with a changed configuration. Without it, a
// long-running process keeps reporting the settings it saw at startup: a tool
// that writes pool settings would never observe its own changes, and would show
// an operator a "currently configured" value that has not been true for hours.
//
// Passing a binary and config path forgets just that pair; passing empty strings
// forgets everything.
func InvalidateConfigCache(binary, configPath string) {
	fpmConfigCacheLock.Lock()
	defer fpmConfigCacheLock.Unlock()

	if binary == "" && configPath == "" {
		clear(fpmConfigCache)

		return
	}

	delete(fpmConfigCache, binary+"::"+configPath)
}

type EffectiveConfig struct {
	Global map[string]string
	Pools  map[string]map[string]string
}

// ParseConfig runs `php-fpm -tt` and parses its report of the effective
// configuration. Results are cached per binary+config pair.
//
// The cache never expires on its own, which is right for the common case — the
// parse forks php-fpm and a scrape loop would otherwise do it every few seconds
// — but it means a caller that CHANGES the configuration has to say so. See
// InvalidateConfigCache.
func ParseConfig(FPMBinaryPath string, FPMConfigPath string) (*EffectiveConfig, error) {
	key := FPMBinaryPath + "::" + FPMConfigPath

	fpmConfigCacheLock.Lock()
	cached, ok := fpmConfigCache[key]
	fpmConfigCacheLock.Unlock()

	if ok {
		return cached, nil
	}

	cmd := exec.Command(FPMBinaryPath, "-tt", "--fpm-config", FPMConfigPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to run php-fpm -tt: %w\nOutput: %s", err, output)
	}

	fpmconfig, err := parseFPMConfigOutput(output)
	if err != nil {
		return nil, err
	}

	fpmConfigCacheLock.Lock()
	fpmConfigCache[key] = fpmconfig
	fpmConfigCacheLock.Unlock()

	return fpmconfig, nil
}

// parseFPMConfigOutput parses the report `php-fpm -tt` writes to stderr. Kept
// free of I/O so it can be driven by a captured fixture: this is the only place
// that knows the shape of that output, and it had no test against real output
// at all.
func parseFPMConfigOutput(output []byte) (*EffectiveConfig, error) {
	fpmconfig := &EffectiveConfig{
		Global: make(map[string]string),
		Pools:  make(map[string]map[string]string),
	}
	currentSection := "global"

	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if idx := strings.Index(line, "] NOTICE:"); idx != -1 {
			line = strings.TrimSpace(line[idx+len("] NOTICE:"):])
		}

		line = strings.ReplaceAll(line, "\\t", "")
		line = strings.ReplaceAll(line, "\t", "")
		line = strings.TrimSpace(strings.Trim(line, `"`))

		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if section == "global" {
				currentSection = "global"
				continue
			}
			currentSection = section
			if _, ok := fpmconfig.Pools[currentSection]; !ok {
				fpmconfig.Pools[currentSection] = make(map[string]string)
			}
			continue
		}

		if strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			// TrimSpace first: the space after `=` used to block the leading
			// quote from being stripped, leaving values like `"50`. php-fpm -tt
			// normalises quotes away so nothing hit it in practice, but the
			// ordering was still wrong -- and the test accepted either result.
			key := strings.Trim(strings.TrimSpace(parts[0]), `"`)
			val := strings.Trim(strings.TrimSpace(parts[1]), `"`)
			if val == "undefined" {
				val = ""
			}

			if currentSection != "global" {
				if _, ok := fpmconfig.Pools[currentSection]; !ok {
					fpmconfig.Pools[currentSection] = make(map[string]string)
				}
				fpmconfig.Pools[currentSection][key] = val
			} else {
				fpmconfig.Global[key] = val
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan php-fpm config output: %w", err)
	}

	return fpmconfig, nil
}
