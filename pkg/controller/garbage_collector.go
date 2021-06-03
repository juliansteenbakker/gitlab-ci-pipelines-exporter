package controller

import (
	"context"
	"reflect"
	"regexp"

	"github.com/mvisonneau/gitlab-ci-pipelines-exporter/pkg/schemas"
	"github.com/mvisonneau/gitlab-ci-pipelines-exporter/pkg/store"
	log "github.com/sirupsen/logrus"
)

// GarbageCollectProjects ..
func (c *Controller) GarbageCollectProjects(_ context.Context) error {
	log.Info("starting 'projects' garbage collection")
	defer log.Info("ending 'projects' garbage collection")

	storedProjects, err := c.Store.Projects()
	if err != nil {
		return err
	}

	// Loop through all configured projects
	for _, cp := range c.Config.Projects {
		p := schemas.Project{Project: cp}
		delete(storedProjects, p.Key())
	}

	// Loop through what can be found from the wildcards
	for _, w := range c.Config.Wildcards {
		foundProjects, err := c.Gitlab.ListProjects(w)
		if err != nil {
			return err
		}

		for _, p := range foundProjects {
			delete(storedProjects, p.Key())
		}
	}

	log.WithFields(log.Fields{
		"projects-count": len(storedProjects),
	}).Debug("found projects to garbage collect")

	for k, p := range storedProjects {
		if err = c.Store.DelProject(k); err != nil {
			return err
		}

		log.WithFields(log.Fields{
			"project-name": p.Name,
		}).Info("deleted project from the store")
	}

	return nil
}

// GarbageCollectEnvironments ..
func (c *Controller) GarbageCollectEnvironments(_ context.Context) error {
	log.Info("starting 'environments' garbage collection")
	defer log.Info("ending 'environments' garbage collection")

	storedEnvironments, err := c.Store.Environments()
	if err != nil {
		return err
	}

	envProjects := make(map[string]string)
	for k, env := range storedEnvironments {
		p := schemas.NewProject(env.ProjectName)

		projectExists, err := c.Store.ProjectExists(p.Key())
		if err != nil {
			return err
		}

		// If the project does not exist anymore, delete the environment
		if !projectExists {
			if err = c.Store.DelEnvironment(k); err != nil {
				return err
			}

			log.WithFields(log.Fields{
				"project-name":     env.ProjectName,
				"environment-name": env.Name,
				"reason":           "non-existent-project",
			}).Info("deleted environment from the store")
			continue
		}

		if err = c.Store.GetProject(&p); err != nil {
			return err
		}

		// Store the project information to be able to refresh its environments
		// from the API later on
		envProjects[p.Name] = p.Pull.Environments.Regexp

		// If the environment is not configured to be pulled anymore, delete it
		re := regexp.MustCompile(p.Pull.Environments.Regexp)

		if !re.MatchString(env.Name) {
			if err = c.Store.DelEnvironment(k); err != nil {
				return err
			}

			log.WithFields(log.Fields{
				"project-name":     env.ProjectName,
				"environment-name": env.Name,
				"reason":           "environment-not-in-regexp",
			}).Info("deleted environment from the store")
			continue
		}

		// Check if the latest configuration of the project in store matches the environment one
		if env.OutputSparseStatusMetrics != p.OutputSparseStatusMetrics {
			env.OutputSparseStatusMetrics = p.OutputSparseStatusMetrics

			if err = c.Store.SetEnvironment(env); err != nil {
				return err
			}

			log.WithFields(log.Fields{
				"project-name":     env.ProjectName,
				"environment-name": env.Name,
			}).Info("updated ref, associated project configuration was not in sync")
		}
	}

	// Refresh the environments from the API
	existingEnvs := make(map[schemas.EnvironmentKey]struct{})
	for projectName, envRegexp := range envProjects {
		envs, err := c.Gitlab.GetProjectEnvironments(projectName, envRegexp)
		if err != nil {
			return err
		}

		for _, envName := range envs {
			existingEnvs[schemas.Environment{
				ProjectName: projectName,
				Name:        envName,
			}.Key()] = struct{}{}
		}
	}

	storedEnvironments, err = c.Store.Environments()
	if err != nil {
		return err
	}

	for k, env := range storedEnvironments {
		if _, exists := existingEnvs[k]; !exists {
			if err = c.Store.DelEnvironment(k); err != nil {
				return err
			}

			log.WithFields(log.Fields{
				"project-name":     env.ProjectName,
				"environment-name": env.Name,
				"reason":           "non-existent-environment",
			}).Info("deleted environment from the store")
		}
	}

	return nil
}

// GarbageCollectRefs ..
func (c *Controller) GarbageCollectRefs(_ context.Context) error {
	log.Info("starting 'refs' garbage collection")
	defer log.Info("ending 'refs' garbage collection")

	storedRefs, err := c.Store.Refs()
	if err != nil {
		return err
	}

	for _, ref := range storedRefs {
		projectExists, err := c.Store.ProjectExists(ref.Project.Key())
		if err != nil {
			return err
		}

		// If the project does not exist anymore, delete the ref
		if !projectExists {
			if err = deleteRef(c.Store, ref, "non-existent-project"); err != nil {
				return err
			}
			continue
		}

		// If the ref is not configured to be pulled anymore, delete the ref
		var re *regexp.Regexp
		if re, err = schemas.GetRefRegexp(ref.Project.Pull.Refs, ref.Kind); err != nil {
			if err = deleteRef(c.Store, ref, "invalid-ref-kind"); err != nil {
				return err
			}
		}

		if !re.MatchString(ref.Name) {
			if err = deleteRef(c.Store, ref, "ref-not-matching-regexp"); err != nil {
				return err
			}
		}

		// Check if the latest configuration of the project in store matches the ref one
		p := ref.Project
		if err = c.Store.GetProject(&p); err != nil {
			return err
		}

		if !reflect.DeepEqual(ref.Project, p) {
			ref.Project = p
			if err = c.Store.SetRef(ref); err != nil {
				return err
			}
			log.WithFields(log.Fields{
				"project-name": ref.Project.Name,
				"ref":          ref.Name,
			}).Info("updated ref, associated project configuration was not in sync")
		}
	}

	// Refresh the refs from the API
	projects, err := c.Store.Projects()
	if err != nil {
		return err
	}

	expectedRefs := make(map[schemas.RefKey]bool)
	for _, p := range projects {
		refs, err := c.GetRefs(p)
		if err != nil {
			return err
		}

		for _, ref := range refs {
			expectedRefs[ref.Key()] = true
		}
	}

	// Refresh the stored refs as we may have already removed some
	storedRefs, err = c.Store.Refs()
	if err != nil {
		return err
	}

	for k, ref := range storedRefs {
		if _, expected := expectedRefs[k]; !expected {
			if err = deleteRef(c.Store, ref, "not-expected"); err != nil {
				return err
			}
		}
	}

	return nil
}

func deleteRef(s store.Store, ref schemas.Ref, reason string) (err error) {
	if err = s.DelRef(ref.Key()); err != nil {
		return
	}

	log.WithFields(log.Fields{
		"project-name": ref.Project.Name,
		"ref":          ref.Name,
		"ref-kind":     ref.Kind,
		"reason":       reason,
	}).Info("deleted ref from the store")

	return
}

// GarbageCollectMetrics ..
func (c *Controller) GarbageCollectMetrics(_ context.Context) error {
	log.Info("starting 'metrics' garbage collection")
	defer log.Info("ending 'metrics' garbage collection")

	storedEnvironments, err := c.Store.Environments()
	if err != nil {
		return err
	}

	storedRefs, err := c.Store.Refs()
	if err != nil {
		return err
	}

	storedMetrics, err := c.Store.Metrics()
	if err != nil {
		return err
	}

	for k, m := range storedMetrics {
		// In order to save some memory space we chose to have to recompose
		// the Ref the metric belongs to
		metricLabelProject, metricLabelProjectExists := m.Labels["project"]
		metricLabelRef, metricLabelRefExists := m.Labels["ref"]
		metricLabelEnvironment, metricLabelEnvironmentExists := m.Labels["environment"]

		if !metricLabelProjectExists || (!metricLabelRefExists && !metricLabelEnvironmentExists) {
			if err = c.Store.DelMetric(k); err != nil {
				return err
			}

			log.WithFields(log.Fields{
				"metric-kind":   m.Kind,
				"metric-labels": m.Labels,
				"reason":        "project-or-ref-and-environment-label-undefined",
			}).Info("deleted metric from the store")
		}

		if metricLabelRefExists && !metricLabelEnvironmentExists {
			refKey := schemas.NewRef(
				schemas.NewProject(metricLabelProject),
				schemas.RefKind(m.Labels["kind"]),
				metricLabelRef,
			).Key()

			ref, refExists := storedRefs[refKey]

			// If the ref does not exist anymore, delete the metric
			if !refExists {
				if err = c.Store.DelMetric(k); err != nil {
					return err
				}

				log.WithFields(log.Fields{
					"metric-kind":   m.Kind,
					"metric-labels": m.Labels,
					"reason":        "non-existent-ref",
				}).Info("deleted metric from the store")
				continue
			}

			// Check if the pulling of jobs related metrics has been disabled
			switch m.Kind {
			case schemas.MetricKindJobArtifactSizeBytes,
				schemas.MetricKindJobDurationSeconds,
				schemas.MetricKindJobID,
				schemas.MetricKindJobRunCount,
				schemas.MetricKindJobStatus,
				schemas.MetricKindJobTimestamp:

				if !ref.Project.Pull.Pipeline.Jobs.Enabled {
					if err = c.Store.DelMetric(k); err != nil {
						return err
					}

					log.WithFields(log.Fields{
						"metric-kind":   m.Kind,
						"metric-labels": m.Labels,
						"reason":        "jobs-metrics-disabled-on-ref",
					}).Info("deleted metric from the store")
					continue
				}

			default:
			}

			// Check if 'output sparse statuses metrics' has been enabled
			switch m.Kind {
			case schemas.MetricKindJobStatus,
				schemas.MetricKindStatus:

				if ref.Project.OutputSparseStatusMetrics && m.Value != 1 {
					if err = c.Store.DelMetric(k); err != nil {
						return err
					}

					log.WithFields(log.Fields{
						"metric-kind":   m.Kind,
						"metric-labels": m.Labels,
						"reason":        "output-sparse-metrics-enabled-on-ref",
					}).Info("deleted metric from the store")
					continue
				}

			default:
			}
		}

		if metricLabelEnvironmentExists {
			envKey := schemas.Environment{
				ProjectName: metricLabelProject,
				Name:        metricLabelEnvironment,
			}.Key()

			env, envExists := storedEnvironments[envKey]

			// If the ref does not exist anymore, delete the metric
			if !envExists {
				if err = c.Store.DelMetric(k); err != nil {
					return err
				}

				log.WithFields(log.Fields{
					"metric-kind":   m.Kind,
					"metric-labels": m.Labels,
					"reason":        "non-existent-environment",
				}).Info("deleted metric from the store")
				continue
			}

			// Check if 'output sparse statuses metrics' has been enabled
			switch m.Kind {
			case schemas.MetricKindEnvironmentDeploymentStatus:
				if env.OutputSparseStatusMetrics && m.Value != 1 {
					if err = c.Store.DelMetric(k); err != nil {
						return err
					}

					log.WithFields(log.Fields{
						"metric-kind":   m.Kind,
						"metric-labels": m.Labels,
						"reason":        "output-sparse-metrics-enabled-on-environment",
					}).Info("deleted metric from the store")
					continue
				}
			}
		}
	}

	return nil
}
