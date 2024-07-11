package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/alecthomas/kingpin"
	"github.com/rs/zerolog/log"
	gdb2cm "github.com/wy100101/gdb2cm/pkg"
	pr2porm "github.com/wy100101/pr2porm/pkg"
	"gopkg.in/yaml.v3"
)

var (
	dashboardsDirGlob                    = kingpin.Flag("dir.dashboards", "Glob of directories with Grafana dashboard JSON files to convert.").String()
	rulesDirGlob                         = kingpin.Flag("dir.rules", "Glob of directories with Grafana dashboard JSON files to convert.").String()
	manifestsDir                         = kingpin.Flag("dir.output", "Output directory for the dashboard configmaps.").Short('m').Required().ExistingDir()
	k8sAnnotations                       = kingpin.Flag("k8s.annotations", "Add an annotation to add the manifests (key=value)").Short('a').StringMap()
	k8sNamespace                         = kingpin.Flag("k8s.namespace", "k8s namespace for generated manifests").Short('n').Default("monitoring").String()
	k8sLabels                            = kingpin.Flag("k8s.labels", "labels to add to the k8s manifests.").Short('l').StringMap()
	rulesLabelsNoEnforceTeams            = kingpin.Flag("metadata.rulesLabelsNoEnforceTeams", "Enforce required team label from dir name for each rule in a rules file.").Short('r').Strings()
	isStringSpecialLowerCaseAlphaNumeric = regexp.MustCompile(`^[a-z0-9][a-z0-9-.]*[a-z0-9]$`).MatchString
)

type kustomizeFile struct {
	APIVersion        string            `yaml:"apiVersion"`
	Kind              string            `yaml:"kind"`
	CommonAnnotations map[string]string `yaml:"commonAnnotations,omitempty"`
	Bases             []string          `yaml:"bases,omitempty"`
	Resources         []string          `yaml:"resources,omitempty"`
}

type Config struct {
	ManifestsDir             string
	K8sAnnotations           *map[string]string
	K8sNamespace             string
	K8sLabels                *map[string]string
	RulesLabelsNoEnforceTeam *map[string]bool
}

type DirProcessor func(dir string, c Config) error

func cleanDir(dir string) error {
	err := os.RemoveAll(dir)
	if err != nil {
		return err
	}
	err = os.MkdirAll(dir, 0775)
	if err != nil {
		return err
	}
	return nil
}

func generateKustomizeResources(dir string) ([]string, error) {
	resources := []string{}
	d, err := os.Open(dir)
	if err != nil {
		return []string{}, err
	}
	defer d.Close()
	entries, err := d.Readdirnames(-1)
	if err != nil {
		return []string{}, err
	}
	for _, entry := range entries {
		if filepath.Ext(entry) == ".yaml" {
			resources = append(resources, entry)
		}
	}
	sort.Strings(resources)

	return resources, nil
}

func generateKustomizeFile(dir string) error {
	r, err := generateKustomizeResources(dir)
	if err != nil {
		return err
	}
	k := kustomizeFile{
		Kind:       "Kustomization",
		APIVersion: "kustomize.config.k8s.io/v1beta1",
		Resources:  r,
	}
	b := bytes.Buffer{}
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	err = e.Encode(&k)
	if err != nil {
		return err
	}
	kf := filepath.Join(dir, "kustomization.yaml")
	err = os.WriteFile(kf, b.Bytes(), 0666)
	return err
}

func getTeamFromFullPath(p string) (t, tp string) {
	t = filepath.Base(filepath.Dir(p))
	tp = fmt.Sprintf("%s-", t)
	return
}

func validateManifestName(pn string) (bool, error) {
	if len(pn) > 253 {
		return false, fmt.Errorf("%s is longer than 253 characters", pn)
	}
	if !isStringSpecialLowerCaseAlphaNumeric(pn) {
		return false, fmt.Errorf("%s should contain only contain special characters -. and begin/end with alphanumeric characters", pn)
	}
	return true, nil
}

func copyMap(m *map[string]string) *map[string]string {
	nm := &map[string]string{}
	for k, v := range *m {
		(*nm)[k] = v
	}
	return nm
}

func processDirs(glob string, c Config, dp DirProcessor) error {
	dirs, err := filepath.Glob(glob)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		err := dp(dir, c)
		if err != nil {
			return err
		}
	}
	return nil
}

func processDashboardDir(d string, c Config) error {
	t, tp := getTeamFromFullPath(d)
	as := copyMap(c.K8sAnnotations)
	(*as)["team"] = t
	(*as)["grafana.org/folder"] = t
	rls := copyMap(c.K8sLabels)
	(*rls)["team"] = t

	err := filepath.Walk(d, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() || filepath.Ext(path) != ".json" {
			if !info.IsDir() && info.Name() != "README.md" {
				log.Info().Msgf("%s is not .json, skipping...", path)
			}
			return nil
		}
		n := strings.TrimSuffix(filepath.Base(path), ".json")
		pn := fmt.Sprintf("%s%s", tp, n)
		mp := filepath.Join(c.ManifestsDir, fmt.Sprintf("%s.db.configmap.yaml", pn))
		_, err = validateManifestName(pn)
		if err != nil {
			return err
		}
		err = gdb2cm.ProcessDashboardFile(path, mp, c.K8sNamespace, pn, true, as)
		if err != nil {
			return fmt.Errorf("%s is not valid: %v", path, err)
		}
		appendPath(path, &c)
		return nil
	})
	return err
}

func processRulesDir(d string, c Config) error {
	t, tp := getTeamFromFullPath(d)
	as := copyMap(c.K8sAnnotations)
	rls := copyMap(c.K8sLabels)

	if !(*c.RulesLabelsNoEnforceTeam)[t] {
		(*rls)["team"] = t
	}
	(*as)["team"] = t

	err := filepath.Walk(d, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() || filepath.Ext(path) != ".yaml" {
			if !info.IsDir() && info.Name() != "README.md" {
				log.Info().Msgf("%s is not .yaml, skipping...", path)
			}
			return nil
		}
		n := strings.TrimSuffix(filepath.Base(path), ".yaml")
		pn := fmt.Sprintf("%s%s", tp, n)
		mp := filepath.Join(c.ManifestsDir, fmt.Sprintf("%s.prometheusrules.yaml", pn))
		_, err = validateManifestName(pn)
		if err != nil {
			return err
		}
		err = pr2porm.ProcessRulesFile(path, mp, c.K8sNamespace, pn, rls, as)
		if err != nil {
			return fmt.Errorf("%s is not valid: %v", path, err)
		}
		appendPath(path, &c)
		return nil
	})
	return err
}

// appendPath writes a string to the .manifests file in the manifestsDir
// this is used for pre-commit, which fails to track new files in some cases
// the output of this file should be detirministic, because
// filepath.Walk is used to walk the directories, and that function works in lexical order
func appendPath(p string, c *Config) {
	f, err := os.OpenFile(filepath.Join(c.ManifestsDir, ".manifests"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal().Msg(fmt.Sprintf("failed to append to .manifests: %s", err.Error()))
	}
	defer f.Close()
	if _, err := f.WriteString(fmt.Sprintf("%s\n", p)); err != nil {
		log.Fatal().Msg(fmt.Sprintf("failed to append to .manifests: %s", err.Error()))
	}
}

func main() {
	var err error
	log.Logger = log.With().Caller().Logger()
	kingpin.Parse()

	err = cleanDir(*manifestsDir)
	if err != nil {
		log.Fatal().Msg(fmt.Sprintf("failed to clean manifestDir: %s", err.Error()))
	}

	c := Config{
		ManifestsDir:             *manifestsDir,
		K8sNamespace:             *k8sNamespace,
		K8sAnnotations:           k8sAnnotations,
		K8sLabels:                k8sLabels,
		RulesLabelsNoEnforceTeam: &map[string]bool{},
	}

	for _, t := range *rulesLabelsNoEnforceTeams {
		(*c.RulesLabelsNoEnforceTeam)[t] = true
	}

	if *dashboardsDirGlob != "" {
		err = processDirs(*dashboardsDirGlob, c, processDashboardDir)
		if err != nil {
			log.Fatal().Err(err).Msg("")
		}
	}

	if *rulesDirGlob != "" {
		err = processDirs(*rulesDirGlob, c, processRulesDir)
		if err != nil {
			log.Fatal().Err(err).Msg("")
		}
	}

	err = generateKustomizeFile(*manifestsDir)
	if err != nil {
		log.Fatal().Err(err).Msg("")
	}
}
