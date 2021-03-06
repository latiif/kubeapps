/*
Copyright (c) 2018 Bitnami

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kubeapps/common/datastore"
	"github.com/kubeapps/kubeapps/pkg/chart/models"
	"github.com/kubeapps/kubeapps/pkg/dbutils"
	_ "github.com/lib/pq"
)

type postgresAssetManager struct {
	*dbutils.PostgresAssetManager
}

func newPGManager(config datastore.Config) (assetManager, error) {
	m, err := dbutils.NewPGManager(config)
	if err != nil {
		return nil, err
	}
	return &postgresAssetManager{m}, nil
}

// Syncing is performed in the following steps:
// 1. Update database to match chart metadata from index
// 2. Concurrently process icons for charts (concurrently)
// 3. Concurrently process the README and values.yaml for the latest chart version of each chart
// 4. Concurrently process READMEs and values.yaml for historic chart versions
//
// These steps are processed in this way to ensure relevant chart data is
// imported into the database as fast as possible. E.g. we want all icons for
// charts before fetching readmes for each chart and version pair.
func (m *postgresAssetManager) Sync(repo models.RepoInternal, charts []models.Chart) error {
	m.InitTables()

	// Ensure the repo exists so FK constraints will be met.
	_, err := m.EnsureRepoExists(repo.Namespace, repo.Name)
	if err != nil {
		return err
	}

	err = m.importCharts(charts, models.Repo{Namespace: repo.Namespace, Name: repo.Name})
	if err != nil {
		return err
	}

	// Remove charts no longer existing in index
	return m.removeMissingCharts(charts)
}

func (m *postgresAssetManager) RepoAlreadyProcessed(repoName, repoChecksum string) bool {
	var lastChecksum string
	row := m.DB.QueryRow(fmt.Sprintf("SELECT checksum FROM %s WHERE name = $1", dbutils.RepositoryTable), repoName)
	if row != nil {
		err := row.Scan(&lastChecksum)
		return err == nil && lastChecksum == repoChecksum
	}
	return false
}

func (m *postgresAssetManager) UpdateLastCheck(repoNamespace, repoName, checksum string, now time.Time) error {
	query := fmt.Sprintf(`INSERT INTO %s (namespace, name, checksum, last_update)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (namespace, name)
	DO UPDATE SET last_update = $4, checksum = $3
	`, dbutils.RepositoryTable)
	rows, err := m.DB.Query(query, repoNamespace, repoName, checksum, now.String())
	if rows != nil {
		defer rows.Close()
	}
	return err
}

func (m *postgresAssetManager) importCharts(charts []models.Chart, repo models.Repo) error {
	for _, chart := range charts {
		d, err := json.Marshal(chart)
		if err != nil {
			return err
		}
		_, err = m.DB.Exec(fmt.Sprintf(`INSERT INTO %s (repo_namespace, repo_name, chart_id, info)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (chart_id, repo_namespace, repo_name)
		DO UPDATE SET info = $4
		`, dbutils.ChartTable), repo.Namespace, repo.Name, chart.ID, string(d))
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *postgresAssetManager) removeMissingCharts(charts []models.Chart) error {
	var chartIDs []string
	for _, chart := range charts {
		chartIDs = append(chartIDs, fmt.Sprintf("'%s'", chart.ID))
	}
	chartIDsString := strings.Join(chartIDs, ", ")
	rows, err := m.DB.Query(fmt.Sprintf("DELETE FROM %s WHERE info ->> 'ID' NOT IN (%s) AND info -> 'repo' ->> 'name' = $1", dbutils.ChartTable, chartIDsString), charts[0].Repo.Name)
	if rows != nil {
		defer rows.Close()
	}
	return err
}

func (m *postgresAssetManager) Delete(repo models.Repo) error {
	rows, err := m.DB.Query(fmt.Sprintf("DELETE FROM %s WHERE name = $1 AND namespace = $2", dbutils.RepositoryTable), repo.Name, repo.Namespace)
	if rows != nil {
		defer rows.Close()
	}
	return err
}

func (m *postgresAssetManager) updateIcon(data []byte, contentType, ID string) error {
	rows, err := m.DB.Query(fmt.Sprintf(
		`UPDATE charts SET info = info || '{"raw_icon": "%s", "icon_content_type": "%s"}'  WHERE info ->> 'ID' = '%s'`,
		base64.StdEncoding.EncodeToString(data), contentType, ID,
	))
	if rows != nil {
		rows.Close()
	}
	return err
}

func (m *postgresAssetManager) filesExist(chartFilesID, digest string) bool {
	rows, err := m.DB.Query(
		fmt.Sprintf("SELECT * FROM %s WHERE chart_files_id = $1 AND info ->> 'Digest' = $2", dbutils.ChartFilesTable),
		chartFilesID,
		digest,
	)
	hasEntries := false
	if rows != nil {
		defer rows.Close()
		hasEntries = rows.Next()
	}
	return err == nil && hasEntries
}

func (m *postgresAssetManager) insertFiles(chartFilesID string, files models.ChartFiles) error {
	if files.Repo == nil {
		return fmt.Errorf("unable to insert file without repo: %q", files.ID)
	}
	query := fmt.Sprintf(`INSERT INTO %s (repo_name, repo_namespace, chart_files_ID, info)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (repo_namespace, chart_files_ID)
	DO UPDATE SET info = $4
	`, dbutils.ChartFilesTable)
	rows, err := m.DB.Query(query, files.Repo.Name, files.Repo.Namespace, chartFilesID, files)
	if rows != nil {
		defer rows.Close()
	}
	return err
}
