// Sweeper Lambda — terminates EC2 instances tagged
// knative-agents-e2e=L2 that have been running longer than
// MAX_AGE_SECONDS. Fired every 30 min by EventBridge.
//
// Cross-compiled to bootstrap (provided.al2023 runtime, arm64) by:
//
//	GOOS=linux GOARCH=arm64 go build -o bootstrap ./
//
// (run from this directory; Terraform packages the bootstrap into
// the Lambda zip via archive_file).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type Result struct {
	Inspected   int      `json:"inspected"`
	Terminated  []string `json:"terminated"`
	MaxAgeHours float64  `json:"max_age_hours"`
}

func handler(ctx context.Context) (Result, error) {
	maxAge, _ := strconv.Atoi(os.Getenv("MAX_AGE_SECONDS"))
	if maxAge == 0 {
		maxAge = 3600
	}
	cutoff := time.Now().Add(-time.Duration(maxAge) * time.Second)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("load aws config: %w", err)
	}
	c := ec2.NewFromConfig(cfg)

	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:knative-agents-e2e"), Values: []string{"L2"}},
			{Name: aws.String("instance-state-name"), Values: []string{
				"pending", "running", "stopping", "stopped",
			}},
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("describe: %w", err)
	}

	var stale []string
	inspected := 0
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			inspected++
			if i.LaunchTime != nil && i.LaunchTime.Before(cutoff) {
				stale = append(stale, *i.InstanceId)
			}
		}
	}

	if len(stale) > 0 {
		_, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: stale,
		})
		if err != nil {
			return Result{}, fmt.Errorf("terminate: %w", err)
		}
		log.Printf("terminated %d instances: %v", len(stale), stale)
	}

	return Result{
		Inspected:   inspected,
		Terminated:  stale,
		MaxAgeHours: float64(maxAge) / 3600,
	}, nil
}

func main() { lambda.Start(handler) }
