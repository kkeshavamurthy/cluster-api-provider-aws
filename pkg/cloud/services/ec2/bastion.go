/*
Copyright 2018 The Kubernetes Authors.

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

package ec2

import (
	"encoding/base64"
	"fmt"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util/conditions"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"
	infrav1 "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha3"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/awserrors"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/filter"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/userdata"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/record"
)

const (
	defaultSSHKeyName = "default"
)

var (
	fallbackBastionInstanceType        = "t3.micro"
	fallbackBastionUsEast1InstanceType = "t2.micro"
)

// ReconcileBastion ensures a bastion is created for the cluster
func (s *Service) ReconcileBastion() error {
	if !s.scope.Bastion().Enabled {
		s.scope.V(4).Info("Skipping bastion reconcile")
		_, err := s.describeBastionInstance()
		if err != nil {
			if awserrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		return s.DeleteBastion()
	}

	s.scope.V(2).Info("Reconciling bastion host")

	subnets := s.scope.Subnets()
	if len(subnets.FilterPrivate()) == 0 {
		s.scope.V(2).Info("No private subnets available, skipping bastion host")
		return nil
	} else if len(subnets.FilterPublic()) == 0 {
		return errors.New("failed to reconcile bastion host, no public subnets are available")
	}

	// Describe bastion instance, if any.
	instance, err := s.describeBastionInstance()
	if awserrors.IsNotFound(err) { // nolint:nestif
		if !conditions.Has(s.scope.InfraCluster(), infrav1.BastionHostReadyCondition) {
			conditions.MarkFalse(s.scope.InfraCluster(), infrav1.BastionHostReadyCondition, infrav1.BastionCreationStartedReason, clusterv1.ConditionSeverityInfo, "")
			if err := s.scope.PatchObject(); err != nil {
				return errors.Wrap(err, "failed to patch conditions")
			}
		}
		instance, err = s.runInstance("bastion", s.getDefaultBastion(s.scope.Bastion().InstanceType, s.scope.Bastion().AMI))
		if err != nil {
			record.Warnf(s.scope.InfraCluster(), "FailedCreateBastion", "Failed to create bastion instance: %v", err)
			return err
		}

		record.Eventf(s.scope.InfraCluster(), "SuccessfulCreateBastion", "Created bastion instance %q", instance.ID)
		s.scope.V(2).Info("Created new bastion host", "instance", instance)

	} else if err != nil {
		return err
	}

	// TODO(vincepri): check for possible changes between the default spec and the instance.

	s.scope.SetBastionInstance(instance.DeepCopy())
	conditions.MarkTrue(s.scope.InfraCluster(), infrav1.BastionHostReadyCondition)
	s.scope.V(2).Info("Reconcile bastion completed successfully")

	return nil
}

// DeleteBastion deletes the Bastion instance
func (s *Service) DeleteBastion() error {
	instance, err := s.describeBastionInstance()
	if err != nil {
		if awserrors.IsNotFound(err) {
			s.scope.V(4).Info("bastion instance does not exist")
			return nil
		}
		return errors.Wrap(err, "unable to describe bastion instance")
	}

	if err := s.TerminateInstanceAndWait(instance.ID); err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedTerminateBastion", "Failed to terminate bastion instance %q: %v", instance.ID, err)
		return errors.Wrap(err, "unable to delete bastion instance")
	}
	record.Eventf(s.scope.InfraCluster(), "SuccessfulTerminateBastion", "Terminated bastion instance %q", instance.ID)

	return nil
}

func (s *Service) describeBastionInstance() (*infrav1.Instance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			filter.EC2.ProviderRole(infrav1.BastionRoleTagValue),
			filter.EC2.Cluster(s.scope.Name()),
			filter.EC2.InstanceStates(
				ec2.InstanceStateNamePending,
				ec2.InstanceStateNameRunning,
				ec2.InstanceStateNameStopping,
				ec2.InstanceStateNameStopped,
			),
		},
	}

	out, err := s.EC2Client.DescribeInstances(input)
	if err != nil {
		record.Eventf(s.scope.InfraCluster(), "FailedDescribeBastionHost", "Failed to describe bastion host: %v", err)
		return nil, errors.Wrap(err, "failed to describe bastion host")
	}

	// TODO: properly handle multiple bastions found rather than just returning
	// the first non-terminated.
	for _, res := range out.Reservations {
		for _, instance := range res.Instances {
			if aws.StringValue(instance.State.Name) != ec2.InstanceStateNameTerminated {
				return s.SDKToInstance(instance)
			}
		}
	}

	return nil, awserrors.NewNotFound("bastion host not found")
}

func (s *Service) getDefaultBastion(instanceType, ami string) *infrav1.Instance {
	name := fmt.Sprintf("%s-bastion", s.scope.Name())
	userData, _ := userdata.NewBastion(&userdata.BastionInput{})

	// If SSHKeyName WAS NOT provided, use the defaultSSHKeyName
	keyName := s.scope.SSHKeyName()
	if keyName == nil {
		keyName = aws.String(defaultSSHKeyName)
	}

	subnet := s.scope.Subnets().FilterPublic()[0]

	if instanceType == "" {
		if strings.Contains(subnet.AvailabilityZone, "us-east-1") {
			instanceType = fallbackBastionUsEast1InstanceType
		} else {
			instanceType = fallbackBastionInstanceType
		}
	}

	if ami == "" {
		ami = s.defaultBastionAMILookup(s.scope.Region())
	}

	i := &infrav1.Instance{
		Type:       instanceType,
		SubnetID:   subnet.ID,
		ImageID:    ami,
		SSHKeyName: keyName,
		UserData:   aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
		SecurityGroupIDs: []string{
			s.scope.Network().SecurityGroups[infrav1.SecurityGroupBastion].ID,
		},
		Tags: infrav1.Build(infrav1.BuildParams{
			ClusterName: s.scope.Name(),
			Lifecycle:   infrav1.ResourceLifecycleOwned,
			Name:        aws.String(name),
			Role:        aws.String(infrav1.BastionRoleTagValue),
			Additional:  s.scope.AdditionalTags(),
		}),
	}

	return i
}
