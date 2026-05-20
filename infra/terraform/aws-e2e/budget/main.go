// Nuke Lambda — fires when AWS Budget hits 100% of the monthly cap.
// Terminates EVERY EC2 instance tagged smol-agents-e2e=*
// regardless of age, then dumps a JSON summary to CloudWatch Logs.
//
// Triggered via SNS subscription from the budget topic.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type Result struct {
	SNSSubject string   `json:"sns_subject"`
	Terminated []string `json:"terminated"`
	Inspected  int      `json:"inspected"`
}

func handler(ctx context.Context, ev events.SNSEvent) (Result, error) {
	var subject string
	if len(ev.Records) > 0 {
		subject = ev.Records[0].SNS.Subject
	}
	log.Printf("budget alarm fired: %s", subject)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("load aws config: %w", err)
	}
	c := ec2.NewFromConfig(cfg)

	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag-key"), Values: []string{"smol-agents-e2e"}},
			{Name: aws.String("instance-state-name"), Values: []string{
				"pending", "running", "stopping", "stopped",
			}},
		},
	})
	if err != nil {
		return Result{}, err
	}

	var ids []string
	inspected := 0
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			inspected++
			ids = append(ids, *i.InstanceId)
		}
	}

	if len(ids) > 0 {
		_, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: ids,
		})
		if err != nil {
			return Result{}, err
		}
	}

	res := Result{SNSSubject: subject, Terminated: ids, Inspected: inspected}
	if b, _ := json.Marshal(res); b != nil {
		log.Printf("nuke complete: %s", b)
	}
	return res, nil
}

func main() { lambda.Start(handler) }
