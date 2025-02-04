// Copyright (C) 2022 NHR@FAU, University Erlangen-Nuremberg.
// All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.
package graph

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/99designs/gqlgen/graphql"
	"github.com/ClusterCockpit/cc-backend/internal/graph/model"
	"github.com/ClusterCockpit/cc-backend/internal/metricdata"
	"github.com/ClusterCockpit/cc-backend/pkg/log"
	"github.com/ClusterCockpit/cc-backend/pkg/schema"
)

const MAX_JOBS_FOR_ANALYSIS = 500

// Helper function for the rooflineHeatmap GraphQL query placed here so that schema.resolvers.go is not too full.
func (r *queryResolver) rooflineHeatmap(
	ctx context.Context,
	filter []*model.JobFilter,
	rows int, cols int,
	minX float64, minY float64, maxX float64, maxY float64) ([][]float64, error) {

	jobs, err := r.Repo.QueryJobs(ctx, filter, &model.PageRequest{Page: 1, ItemsPerPage: MAX_JOBS_FOR_ANALYSIS + 1}, nil)
	if err != nil {
		log.Error("Error while querying jobs for roofline")
		return nil, err
	}
	if len(jobs) > MAX_JOBS_FOR_ANALYSIS {
		return nil, fmt.Errorf("GRAPH/STATS > too many jobs matched (max: %d)", MAX_JOBS_FOR_ANALYSIS)
	}

	fcols, frows := float64(cols), float64(rows)
	minX, minY, maxX, maxY = math.Log10(minX), math.Log10(minY), math.Log10(maxX), math.Log10(maxY)
	tiles := make([][]float64, rows)
	for i := range tiles {
		tiles[i] = make([]float64, cols)
	}

	for _, job := range jobs {
		if job.MonitoringStatus == schema.MonitoringStatusDisabled || job.MonitoringStatus == schema.MonitoringStatusArchivingFailed {
			continue
		}

		jobdata, err := metricdata.LoadData(job, []string{"flops_any", "mem_bw"}, []schema.MetricScope{schema.MetricScopeNode}, ctx)
		if err != nil {
			log.Error("Error while loading metrics for roofline")
			return nil, err
		}

		flops_, membw_ := jobdata["flops_any"], jobdata["mem_bw"]
		if flops_ == nil && membw_ == nil {
			return nil, fmt.Errorf("GRAPH/STATS > 'flops_any' or 'mem_bw' missing for job %d", job.ID)
		}

		flops, ok1 := flops_["node"]
		membw, ok2 := membw_["node"]
		if !ok1 || !ok2 {
			// TODO/FIXME:
			return nil, errors.New("GRAPH/STATS > todo: rooflineHeatmap() query not implemented for where flops_any or mem_bw not available at 'node' level")
		}

		for n := 0; n < len(flops.Series); n++ {
			flopsSeries, membwSeries := flops.Series[n], membw.Series[n]
			for i := 0; i < len(flopsSeries.Data); i++ {
				if i >= len(membwSeries.Data) {
					break
				}

				x, y := math.Log10(float64(flopsSeries.Data[i]/membwSeries.Data[i])), math.Log10(float64(flopsSeries.Data[i]))
				if math.IsNaN(x) || math.IsNaN(y) || x < minX || x >= maxX || y < minY || y > maxY {
					continue
				}

				x, y = math.Floor(((x-minX)/(maxX-minX))*fcols), math.Floor(((y-minY)/(maxY-minY))*frows)
				if x < 0 || x >= fcols || y < 0 || y >= frows {
					continue
				}

				tiles[int(y)][int(x)] += 1
			}
		}
	}

	return tiles, nil
}

// Helper function for the jobsFootprints GraphQL query placed here so that schema.resolvers.go is not too full.
func (r *queryResolver) jobsFootprints(ctx context.Context, filter []*model.JobFilter, metrics []string) (*model.Footprints, error) {
	jobs, err := r.Repo.QueryJobs(ctx, filter, &model.PageRequest{Page: 1, ItemsPerPage: MAX_JOBS_FOR_ANALYSIS + 1}, nil)
	if err != nil {
		log.Error("Error while querying jobs for footprint")
		return nil, err
	}
	if len(jobs) > MAX_JOBS_FOR_ANALYSIS {
		return nil, fmt.Errorf("GRAPH/STATS > too many jobs matched (max: %d)", MAX_JOBS_FOR_ANALYSIS)
	}

	avgs := make([][]schema.Float, len(metrics))
	for i := range avgs {
		avgs[i] = make([]schema.Float, 0, len(jobs))
	}

	nodehours := make([]schema.Float, 0, len(jobs))
	for _, job := range jobs {
		if job.MonitoringStatus == schema.MonitoringStatusDisabled || job.MonitoringStatus == schema.MonitoringStatusArchivingFailed {
			continue
		}

		if err := metricdata.LoadAverages(job, metrics, avgs, ctx); err != nil {
			log.Error("Error while loading averages for footprint")
			return nil, err
		}

		nodehours = append(nodehours, schema.Float(float64(job.Duration)/60.0*float64(job.NumNodes)))
	}

	res := make([]*model.MetricFootprints, len(avgs))
	for i, arr := range avgs {
		res[i] = &model.MetricFootprints{
			Metric: metrics[i],
			Data:   arr,
		}
	}

	return &model.Footprints{
		Nodehours: nodehours,
		Metrics:   res,
	}, nil
}

func requireField(ctx context.Context, name string) bool {
	fields := graphql.CollectAllFields(ctx)

	for _, f := range fields {
		if f == name {
			return true
		}
	}

	return false
}
