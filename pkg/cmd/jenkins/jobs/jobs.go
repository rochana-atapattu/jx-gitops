package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/sprig"
	"github.com/jenkins-x-plugins/jx-gitops/pkg/apis/gitops/v1alpha1"
	"github.com/jenkins-x-plugins/jx-gitops/pkg/cmd/jenkins/add"
	"github.com/jenkins-x-plugins/jx-gitops/pkg/rootcmd"
	"github.com/jenkins-x-plugins/jx-gitops/pkg/sourceconfigs"
	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/templater"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-helpers/v3/pkg/yamls"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Generates the Jenkins Jobs helm files
`)

	cmdExample = templates.Examples(`
		# generate the jenkins job files
		%s jenkins jobs

	`)

	jobValuesHeader = `# NOTE this file is autogenerated - DO NOT EDIT!
#
# This file is generated from the template files via the command: 
#    jx gitops jenkins jobs
controller:
  JCasC:
    configScripts:
      jxsetup: |
        jobs:
          - script: |
`
	indent = "              "
)

// LabelOptions the options for the command
type Options struct {
	Dir                    string
	ConfigFile             string
	OutDir                 string
	DefaultTemplate        string
	NoCreateHelmfile       bool
	SourceConfig           v1alpha1.SourceConfig
	JenkinsServerTemplates map[string][]*JenkinsTemplateConfig
}

// JenkinsTemplateConfig stores the data to render jenkins config files
type JenkinsTemplateConfig struct {
	Server       string
	Key          string
	TemplateFile string
	TemplateText string
	TemplateData map[string]interface{}
}

// NewCmdJenkinsJobs creates a command object for the command
func NewCmdJenkinsJobs() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:     "jobs",
		Aliases: []string{"job"},
		Short:   "Generates the Jenkins Jobs helm files",
		Long:    cmdLong,
		Example: fmt.Sprintf(cmdExample, rootcmd.BinaryName),
		Run: func(_ *cobra.Command, _ []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&o.Dir, "dir", "d", ".", "the current working directory")
	cmd.Flags().StringVarP(&o.OutDir, "out", "o", "", "the output directory for the generated config files. If not specified defaults to the jenkins dir in the current directory")
	cmd.Flags().StringVarP(&o.ConfigFile, "config", "c", "", "the configuration file to load for the repository configurations. If not specified we look in ./.jx/gitops/source-config.yaml")
	cmd.Flags().StringVarP(&o.DefaultTemplate, "default-template", "", "", "the default job template file if none is configured for a repository")
	cmd.Flags().BoolVarP(&o.NoCreateHelmfile, "no-create-helmfile", "", false, "disables the creation of the helmfiles/jenkinsName/helmfile.yaml file if a jenkins server does not yet exist")
	return cmd, o
}

func (o *Options) Validate() error {
	if o.ConfigFile == "" {
		o.ConfigFile = filepath.Join(o.Dir, ".jx", "gitops", v1alpha1.SourceConfigFileName)
	}
	if o.OutDir == "" {
		o.OutDir = filepath.Join(o.Dir, "helmfiles")
	}

	exists, err := files.FileExists(o.ConfigFile)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", o.ConfigFile)
	}
	if !exists {
		log.Logger().Infof("the source config file %s does not exist", info(o.ConfigFile))
		return nil
	}

	if o.DefaultTemplate != "" {
		exists, err := files.FileExists(o.DefaultTemplate)
		if err != nil {
			return errors.Wrapf(err, "failed to check if file exists %s", o.DefaultTemplate)
		}
		if !exists {
			return errors.Errorf("the default-xml-template file %s does not exist", o.DefaultTemplate)
		}
	}

	err = yamls.LoadFile(o.ConfigFile, &o.SourceConfig)
	if err != nil {
		return errors.Wrapf(err, "failed to load file %s", o.ConfigFile)
	}

	if o.JenkinsServerTemplates == nil {
		o.JenkinsServerTemplates = map[string][]*JenkinsTemplateConfig{}
	}
	return nil
}

func (o *Options) Run() error {
	err := o.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate options")
	}

	config := &o.SourceConfig

	localJenkinsfilePath := filepath.Join("jenkins", "templates")
	for _, jenkinsTemplatePath := range []string{localJenkinsfilePath, filepath.Join("versionStream", localJenkinsfilePath)} {
		if config.Spec.JenkinsFolderTemplate == "" {
			relPath := filepath.Join(jenkinsTemplatePath, "folder.gotmpl")
			path := filepath.Join(o.Dir, relPath)
			exists, err := files.FileExists(path)
			if err != nil {
				return errors.Wrapf(err, "failed to check if path exists %s", path)
			}
			if exists {
				config.Spec.JenkinsFolderTemplate = relPath
			}
		}
		if config.Spec.JenkinsJobTemplate == "" {
			relPath := filepath.Join(jenkinsTemplatePath, "job.gotmpl")
			path := filepath.Join(o.Dir, relPath)
			exists, err := files.FileExists(path)
			if err != nil {
				return errors.Wrapf(err, "failed to check if path exists %s", path)
			}
			if exists {
				config.Spec.JenkinsJobTemplate = relPath
			}
		}
	}

	for i := range config.Spec.JenkinsServers {
		server := &config.Spec.JenkinsServers[i]
		serverName := server.Server
		for j := range server.Groups {
			group := &server.Groups[j]
			if len(group.Repositories) > 0 {
				jobTemplate := firstNonBlankValue(group.JenkinsFolderTemplate, server.FolderTemplate, config.Spec.JenkinsFolderTemplate)
				err = o.processJenkinsJobConfigForGroup(group, serverName, jobTemplate)
				if err != nil {
					return errors.Wrapf(err, "failed to process Jenkins Config for group %s", group.Owner)
				}
			}
		}
		for j := range server.Groups {
			group := &server.Groups[j]
			for k := range group.Repositories {
				repo := &group.Repositories[k]
				err = sourceconfigs.DefaultValues(config, group, repo)
				if err != nil {
					return errors.Wrap(err, "failed to set default values for jenkins")
				}
				jobTemplate := firstNonBlankValue(repo.JenkinsJobTemplate, group.JenkinsJobTemplate, server.JobTemplate, config.Spec.JenkinsJobTemplate)
				err = o.processJenkinsJobConfigForRepository(group, repo, serverName, jobTemplate)
				if err != nil {
					return errors.Wrapf(err, "failed to process Jenkins Config for repository %s", repo.Name)
				}
			}
		}
	}

	for server, configs := range o.JenkinsServerTemplates {
		dir := filepath.Join(o.OutDir, server)
		err = os.MkdirAll(dir, files.DefaultDirWritePermissions)
		if err != nil {
			return errors.Wrapf(err, "failed to create dir %s", dir)
		}

		err = o.verifyServerHelmfileExists(server)
		if err != nil {
			return errors.Wrapf(err, "failed to verify the jenkins helmfile exists for %s", server)
		}

		path := filepath.Join(dir, "job-values.yaml")
		log.Logger().Infof("creating Jenkins values.yaml file %s", path)

		funcMap := sprig.TxtFuncMap()

		buf := strings.Builder{}
		buf.WriteString(jobValuesHeader)

		for _, jcfg := range configs {
			tmplpath := jcfg.TemplateFile
			output, err := templater.Evaluate(funcMap, jcfg.TemplateData, jcfg.TemplateText, tmplpath, "Jenkins Server "+server)
			if err != nil {
				return errors.Wrapf(err, "failed to evaluate template %s", tmplpath)
			}
			buf.WriteString(indent + "// from template: " + tmplpath + "\n")
			buf.WriteString(indentText(output, indent))
			buf.WriteString(indent + "\n")
		}

		err = os.WriteFile(path, []byte(buf.String()), files.DefaultFileWritePermissions)
		if err != nil {
			return errors.Wrapf(err, "failed to save file %s", path)
		}
	}
	return nil
}

func indentText(text, indent string) string {
	lines := strings.Split(text, "\n")
	return indent + strings.Join(lines, "\n"+indent)
}

func firstNonBlankValue(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (o *Options) processJenkinsJobConfigForGroup(group *v1alpha1.RepositoryGroup, server, jobTemplatePath string) error {
	owner := group.Owner
	if server == "" {
		log.Logger().Infof("ignoring group %s as it has no Jenkins server defined", owner)
		return nil
	}
	if jobTemplatePath == "" {
		log.Logger().Infof("ignoring group %s as it has no Jenkins JobTemplate defined at the repository, group or server level", owner)
		return nil
	}
	templateData := map[string]interface{}{
		"ID":           owner,
		"GitServerURL": group.Provider,
		"GitKind":      group.ProviderKind,
		"GitName":      group.ProviderName,
		"Server":       server,
	}
	return o.processJenkinsServerJobTemplate(server, owner, jobTemplatePath, templateData)
}

func (o *Options) processJenkinsJobConfigForRepository(group *v1alpha1.RepositoryGroup, repo *v1alpha1.Repository, server, jobTemplatePath string) error {
	if server == "" {
		log.Logger().Infof("ignoring repository %s as it has no Jenkins server defined", repo.URL)
		return nil
	}
	if jobTemplatePath == "" {
		log.Logger().Infof("ignoring repository %s as it has no Jenkins JobTemplate defined at the repository, group or server level", repo.URL)
		return nil
	}
	fullName := scm.Join(group.Owner, repo.Name)
	templateData := map[string]interface{}{
		"ID":           group.Owner + "-" + repo.Name,
		"FullName":     fullName,
		"Owner":        group.Owner,
		"GitServerURL": group.Provider,
		"GitKind":      group.ProviderKind,
		"GitName":      group.ProviderName,
		"Repository":   repo.Name,
		"URL":          repo.URL,
		"CloneURL":     repo.HTTPCloneURL,
		"Server":       server,
	}
	return o.processJenkinsServerJobTemplate(server, fullName, jobTemplatePath, templateData)
}

func (o *Options) processJenkinsServerJobTemplate(server, key, jobTemplatePath string, templateData map[string]interface{}) error {
	jobTemplate := filepath.Join(o.Dir, jobTemplatePath)
	exists, err := files.FileExists(jobTemplate)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", jobTemplate)
	}
	if !exists {
		return errors.Errorf("the jobTemplate file %s does not exist", jobTemplate)
	}
	data, err := os.ReadFile(jobTemplate)
	if err != nil {
		return errors.Wrapf(err, "failed to load file %s", jobTemplate)
	}

	o.JenkinsServerTemplates[server] = append(o.JenkinsServerTemplates[server], &JenkinsTemplateConfig{
		Server:       server,
		Key:          key,
		TemplateFile: jobTemplate,
		TemplateText: string(data),
		TemplateData: templateData,
	})
	return nil
}

func (o *Options) verifyServerHelmfileExists(server string) error {
	_, ao := add.NewCmdJenkinsAdd()
	ao.Name = server
	ao.Dir = o.Dir
	ao.Values = []string{"job-values.yaml", "values.yaml"}

	err := ao.Run()
	if err != nil {
		return errors.Wrapf(err, "failed to add jenkins server")
	}
	return nil
}
