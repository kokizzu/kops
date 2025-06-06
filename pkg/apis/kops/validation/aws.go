/*
Copyright 2017 The Kubernetes Authors.

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

package validation

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
)

func awsValidateCluster(c *kops.Cluster, strict bool) field.ErrorList {
	allErrs := field.ErrorList{}

	if c.Spec.API.LoadBalancer != nil {
		lbPath := field.NewPath("spec", "api", "loadBalancer")
		lbSpec := c.Spec.API.LoadBalancer
		if strict || lbSpec.Class != "" {
			allErrs = append(allErrs, IsValidValue(lbPath.Child("class"), &lbSpec.Class, kops.SupportedLoadBalancerClasses)...)
		}
		allErrs = append(allErrs, awsValidateTopologyDNS(lbPath.Child("type"), c)...)
		allErrs = append(allErrs, awsValidateSecurityGroupOverride(lbPath.Child("securityGroupOverride"), lbSpec)...)
		allErrs = append(allErrs, awsValidateAdditionalSecurityGroups(lbPath.Child("additionalSecurityGroups"), lbSpec.AdditionalSecurityGroups)...)
		if lbSpec.Class == kops.LoadBalancerClassNetwork && lbSpec.UseForInternalAPI && lbSpec.Type == kops.LoadBalancerTypeInternal {
			allErrs = append(allErrs, field.Forbidden(lbPath.Child("useForInternalAPI"), "useForInternalAPI cannot be used with internal NLB due lack of hairpinning support"))
		}
		if lbSpec.SSLCertificate != "" && lbSpec.Class != kops.LoadBalancerClassNetwork {
			allErrs = append(allErrs, field.Forbidden(lbPath.Child("sslCertificate"), "sslCertificate requires a network load balancer. See https://github.com/kubernetes/kops/blob/master/permalinks/acm_nlb.md"))
		}
		allErrs = append(allErrs, awsValidateSSLPolicy(lbPath.Child("sslPolicy"), lbSpec)...)
		allErrs = append(allErrs, awsValidateLoadBalancerSubnets(lbPath.Child("subnets"), c.Spec)...)
	}

	allErrs = append(allErrs, awsValidateEBSCSIDriver(c)...)

	if c.Spec.Authentication != nil && c.Spec.Authentication.AWS != nil {
		allErrs = append(allErrs, awsValidateIAMAuthenticator(field.NewPath("spec", "authentication", "aws"), c.Spec.Authentication.AWS)...)
	}

	return allErrs
}

func awsValidateEBSCSIDriver(cluster *kops.Cluster) (allErrs field.ErrorList) {
	c := cluster.Spec

	fldPath := field.NewPath("spec", "cloudProvider", "aws", "ebsCSIDriver", "enabled")
	if c.CloudProvider.AWS.EBSCSIDriver != nil && c.CloudProvider.AWS.EBSCSIDriver.Enabled != nil && !*c.CloudProvider.AWS.EBSCSIDriver.Enabled {
		allErrs = append(allErrs, field.Forbidden(fldPath, "must not be disabled"))
	}
	return allErrs
}

func awsValidateInstanceGroup(ig *kops.InstanceGroup, cloud awsup.AWSCloud) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, awsValidateAdditionalSecurityGroups(field.NewPath("spec", "additionalSecurityGroups"), ig.Spec.AdditionalSecurityGroups)...)

	allErrs = append(allErrs, awsValidateInstanceTypeAndImage(field.NewPath(ig.GetName(), "spec", "machineType"), field.NewPath(ig.GetName(), "spec", "image"), ig.Spec.MachineType, ig.Spec.Image, cloud)...)

	allErrs = append(allErrs, awsValidateSpotDurationInMinute(field.NewPath(ig.GetName(), "spec", "spotDurationInMinutes"), ig)...)

	allErrs = append(allErrs, awsValidateInstanceInterruptionBehavior(field.NewPath(ig.GetName(), "spec", "instanceInterruptionBehavior"), ig)...)

	if ig.Spec.MixedInstancesPolicy != nil {
		allErrs = append(allErrs, awsValidateMixedInstancesPolicy(field.NewPath("spec", "mixedInstancesPolicy"), ig.Spec.MixedInstancesPolicy, ig, cloud)...)
	}

	if ig.Spec.InstanceMetadata != nil {
		allErrs = append(allErrs, awsValidateInstanceMetadata(field.NewPath("spec", "instanceMetadata"), ig.Spec.InstanceMetadata)...)
	}

	if ig.Spec.CPUCredits != nil {
		allErrs = append(allErrs, awsValidateCPUCredits(field.NewPath("spec"), &ig.Spec, cloud)...)
	}

	if ig.Spec.MaxInstanceLifetime != nil {
		allErrs = append(allErrs, awsValidateMaximumInstanceLifetime(field.NewPath(ig.GetName(), "spec"), ig.Spec.MaxInstanceLifetime)...)
	}

	return allErrs
}

func awsValidateMaximumInstanceLifetime(fieldPath *field.Path, maxInstanceLifetime *metav1.Duration) field.ErrorList {
	allErrs := field.ErrorList{}
	const minMaxInstanceLifetime = 86400
	lifetimeSec := int64(maxInstanceLifetime.Seconds())
	if lifetimeSec != 0 && lifetimeSec < minMaxInstanceLifetime {
		allErrs = append(allErrs, field.Invalid(fieldPath.Child("maxInstanceLifetime"), maxInstanceLifetime, fmt.Sprintf("max instance lifetime must be greater than %d or equal to 0", int64(minMaxInstanceLifetime))))
	}

	return allErrs
}

func awsValidateInstanceMetadata(fieldPath *field.Path, instanceMetadata *kops.InstanceMetadataOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if instanceMetadata.HTTPTokens != nil {
		allErrs = append(allErrs, IsValidValue(fieldPath.Child("httpTokens"), instanceMetadata.HTTPTokens, []string{"optional", "required"})...)
	}

	if instanceMetadata.HTTPPutResponseHopLimit != nil {
		httpPutResponseHopLimit := fi.ValueOf(instanceMetadata.HTTPPutResponseHopLimit)
		if httpPutResponseHopLimit < 1 || httpPutResponseHopLimit > 64 {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("httpPutResponseHopLimit"), instanceMetadata.HTTPPutResponseHopLimit,
				"HTTPPutResponseLimit must be a value between 1 and 64"))
		}
	}

	return allErrs
}

func awsValidateAdditionalSecurityGroups(fieldPath *field.Path, groups []string) field.ErrorList {
	allErrs := field.ErrorList{}

	names := sets.NewString()
	for i, s := range groups {
		if names.Has(s) {
			allErrs = append(allErrs, field.Duplicate(fieldPath.Index(i), s))
		}
		names.Insert(s)
		if strings.TrimSpace(s) == "" {
			allErrs = append(allErrs, field.Invalid(fieldPath.Index(i), s, "security group cannot be empty, if specified"))
			continue
		}
		if !strings.HasPrefix(s, "sg-") {
			allErrs = append(allErrs, field.Invalid(fieldPath.Index(i), s, "security group does not match the expected AWS format"))
		}
	}

	return allErrs
}

func awsValidateSecurityGroupOverride(fieldPath *field.Path, lbSpec *kops.LoadBalancerAccessSpec) field.ErrorList {
	if lbSpec.SecurityGroupOverride == nil {
		return nil
	}

	allErrs := field.ErrorList{}

	override := *lbSpec.SecurityGroupOverride
	if strings.TrimSpace(override) == "" {
		return append(allErrs, field.Invalid(fieldPath, override, "security group override cannot be empty, if specified"))
	}
	if !strings.HasPrefix(override, "sg-") {
		allErrs = append(allErrs, field.Invalid(fieldPath, override, "security group override does not match the expected AWS format"))
	}

	return allErrs
}

func awsValidateInstanceTypeAndImage(instanceTypeFieldPath *field.Path, imageFieldPath *field.Path, instanceTypes string, image string, cloud awsup.AWSCloud) field.ErrorList {
	if cloud == nil || instanceTypes == "" {
		return nil
	}

	allErrs := field.ErrorList{}

	imageInfo, err := cloud.ResolveImage(image)
	if err != nil {
		return append(allErrs, field.Invalid(imageFieldPath, image,
			fmt.Sprintf("specified image %q is invalid: %s", image, err)))
	}
	imageArch := string(imageInfo.Architecture)

	// Spotinst uses the instance type field to keep a "," separated list of instance types
	for _, instanceType := range strings.Split(instanceTypes, ",") {
		machineInfo, err := cloud.DescribeInstanceType(instanceType)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(instanceTypeFieldPath, instanceTypes, fmt.Sprintf("machine type %q is invalid: %v", instanceType, err)))
			continue
		}

		found := false
		if machineInfo != nil && machineInfo.ProcessorInfo != nil {
			for _, machineArch := range machineInfo.ProcessorInfo.SupportedArchitectures {
				if imageArch == string(machineArch) {
					found = true
				}
			}
		}
		if !found {
			machineArch := make([]string, 0)
			if machineInfo != nil && machineInfo.ProcessorInfo != nil && machineInfo.ProcessorInfo.SupportedArchitectures != nil {
				for _, arch := range machineInfo.ProcessorInfo.SupportedArchitectures {
					machineArch = append(machineArch, string(arch))
				}
			}
			allErrs = append(allErrs, field.Invalid(instanceTypeFieldPath, instanceTypes,
				fmt.Sprintf("machine type architecture %q does not match image architecture %q", strings.Join(machineArch, ","), imageArch)))
		}
	}

	return allErrs
}

func awsValidateSpotDurationInMinute(fieldPath *field.Path, ig *kops.InstanceGroup) field.ErrorList {
	allErrs := field.ErrorList{}
	if ig.Spec.SpotDurationInMinutes != nil {
		validSpotDurations := []string{"60", "120", "180", "240", "300", "360"}
		spotDurationStr := strconv.FormatInt(*ig.Spec.SpotDurationInMinutes, 10)
		allErrs = append(allErrs, IsValidValue(fieldPath, &spotDurationStr, validSpotDurations)...)
	}
	return allErrs
}

func awsValidateInstanceInterruptionBehavior(fieldPath *field.Path, ig *kops.InstanceGroup) field.ErrorList {
	allErrs := field.ErrorList{}
	if ig.Spec.InstanceInterruptionBehavior != nil {
		instanceInterruptionBehavior := ec2types.InstanceInterruptionBehavior(*ig.Spec.InstanceInterruptionBehavior)
		allErrs = append(allErrs, IsValidValue(fieldPath, &instanceInterruptionBehavior, ec2types.InstanceInterruptionBehavior("").Values())...)
	}
	return allErrs
}

// awsValidateMixedInstancesPolicy is responsible for validating the user input of a mixed instance policy
func awsValidateMixedInstancesPolicy(path *field.Path, spec *kops.MixedInstancesPolicySpec, ig *kops.InstanceGroup, cloud awsup.AWSCloud) field.ErrorList {
	var errs field.ErrorList

	mainMachineTypeInfo, err := awsup.GetMachineTypeInfo(cloud, ec2types.InstanceType(ig.Spec.MachineType))
	if err != nil {
		errs = append(errs, field.Invalid(field.NewPath("spec", "machineType"), ig.Spec.MachineType, fmt.Sprintf("machine type specified is invalid: %q", ig.Spec.MachineType)))
		return errs
	}

	hasGPU := mainMachineTypeInfo.GPU

	// @step: check the instance types are valid
	for i, instanceTypes := range spec.Instances {
		fld := path.Child("instances").Index(i)
		errs = append(errs, awsValidateInstanceTypeAndImage(path.Child("instances").Index(i), path.Child("image"), instanceTypes, ig.Spec.Image, cloud)...)

		for _, instanceType := range strings.Split(instanceTypes, ",") {
			machineTypeInfo, err := awsup.GetMachineTypeInfo(cloud, ec2types.InstanceType(instanceType))
			if err != nil {
				errs = append(errs, field.Invalid(field.NewPath("spec", "machineType"), ig.Spec.MachineType, fmt.Sprintf("machine type specified is invalid: %q", ig.Spec.MachineType)))
				return errs
			}
			if machineTypeInfo.GPU != hasGPU {
				errs = append(errs, field.Forbidden(fld, "Cannot mix GPU and non-GPU machine types in the same Instance Group"))
			}
		}

	}

	if spec.OnDemandBase != nil {
		if fi.ValueOf(spec.OnDemandBase) < 0 {
			errs = append(errs, field.Invalid(path.Child("onDemandBase"), spec.OnDemandBase, "cannot be less than zero"))
		}
		if fi.ValueOf(spec.OnDemandBase) > int64(fi.ValueOf(ig.Spec.MaxSize)) {
			errs = append(errs, field.Invalid(path.Child("onDemandBase"), spec.OnDemandBase, "cannot be greater than max size"))
		}
	}

	if spec.OnDemandAboveBase != nil {
		if fi.ValueOf(spec.OnDemandAboveBase) < 0 {
			errs = append(errs, field.Invalid(path.Child("onDemandAboveBase"), spec.OnDemandAboveBase, "cannot be less than 0"))
		}
		if fi.ValueOf(spec.OnDemandAboveBase) > 100 {
			errs = append(errs, field.Invalid(path.Child("onDemandAboveBase"), spec.OnDemandAboveBase, "cannot be greater than 100"))
		}
	}

	errs = append(errs, IsValidValue(path.Child("spotAllocationStrategy"), spec.SpotAllocationStrategy, kops.SpotAllocationStrategies)...)

	return errs
}

func awsValidateTopologyDNS(fieldPath *field.Path, c *kops.Cluster) field.ErrorList {
	allErrs := field.ErrorList{}

	if c.UsesNoneDNS() && c.Spec.API.LoadBalancer != nil && c.Spec.API.LoadBalancer.Class != kops.LoadBalancerClassNetwork {
		allErrs = append(allErrs, field.Forbidden(fieldPath, "topology.dns.type=none requires Network Load Balancer"))
	}

	return allErrs
}

func awsValidateSSLPolicy(fieldPath *field.Path, spec *kops.LoadBalancerAccessSpec) field.ErrorList {
	allErrs := field.ErrorList{}

	if spec.SSLPolicy != nil {
		if spec.Class != kops.LoadBalancerClassNetwork {
			allErrs = append(allErrs, field.Forbidden(fieldPath, "sslPolicy should be specified with Network Load Balancer"))
		}
		if spec.SSLCertificate == "" {
			allErrs = append(allErrs, field.Forbidden(fieldPath, "sslPolicy should not be specified without SSLCertificate"))
		}
	}

	return allErrs
}

func awsValidateLoadBalancerSubnets(fieldPath *field.Path, spec kops.ClusterSpec) field.ErrorList {
	allErrs := field.ErrorList{}

	lbSpec := spec.API.LoadBalancer

	for i, subnet := range lbSpec.Subnets {
		var clusterSubnet *kops.ClusterSubnetSpec
		if subnet.Name == "" {
			allErrs = append(allErrs, field.Required(fieldPath.Index(i).Child("name"), "subnet name can't be empty"))
		} else {
			for _, cs := range spec.Networking.Subnets {
				if subnet.Name == cs.Name {
					clusterSubnet = &cs
					break
				}
			}
			if clusterSubnet == nil {
				allErrs = append(allErrs, field.NotFound(fieldPath.Index(i).Child("name"), fmt.Sprintf("subnet %q not found in cluster subnets", subnet.Name)))
			}
		}

		if subnet.PrivateIPv4Address != nil {
			if *subnet.PrivateIPv4Address == "" {
				allErrs = append(allErrs, field.Required(fieldPath.Index(i).Child("privateIPv4Address"), "privateIPv4Address can't be empty"))
			}
			ip := net.ParseIP(*subnet.PrivateIPv4Address)
			if ip == nil || ip.To4() == nil {
				allErrs = append(allErrs, field.Invalid(fieldPath.Index(i).Child("privateIPv4Address"), subnet, "privateIPv4Address is not a valid IPv4 address"))
			} else if clusterSubnet != nil {
				_, ipNet, err := net.ParseCIDR(clusterSubnet.CIDR)
				if err == nil { // we assume that the cidr is actually valid
					if !ipNet.Contains(ip) {
						allErrs = append(allErrs, field.Invalid(fieldPath.Index(i).Child("privateIPv4Address"), subnet, "privateIPv4Address is not part of the subnet CIDR"))
					}
				}

			}
			if lbSpec.Class != kops.LoadBalancerClassNetwork || lbSpec.Type != kops.LoadBalancerTypeInternal {
				allErrs = append(allErrs, field.Forbidden(fieldPath.Index(i).Child("privateIPv4Address"), "privateIPv4Address only allowed for internal NLBs"))
			}
		}

		if subnet.AllocationID != nil {
			if *subnet.AllocationID == "" {
				allErrs = append(allErrs, field.Required(fieldPath.Index(i).Child("allocationID"), "allocationID can't be empty"))
			}

			if lbSpec.Class != kops.LoadBalancerClassNetwork || lbSpec.Type == kops.LoadBalancerTypeInternal {
				allErrs = append(allErrs, field.Forbidden(fieldPath.Index(i).Child("allocationID"), "allocationID only allowed for Public NLBs"))
			}
		}
	}

	return allErrs
}

func awsValidateCPUCredits(fieldPath *field.Path, spec *kops.InstanceGroupSpec, cloud awsup.AWSCloud) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, IsValidValue(fieldPath.Child("cpuCredits"), spec.CPUCredits, []string{"standard", "unlimited"})...)
	return allErrs
}

func awsValidateIAMAuthenticator(fieldPath *field.Path, spec *kops.AWSAuthenticationSpec) field.ErrorList {
	allErrs := field.ErrorList{}

	if !strings.Contains(spec.BackendMode, "CRD") && len(spec.IdentityMappings) > 0 {
		allErrs = append(allErrs, field.Forbidden(fieldPath.Child("backendMode"), "backendMode must be CRD if identityMappings is set"))
	}
	for i, mapping := range spec.IdentityMappings {
		parsedARN, err := arn.Parse(mapping.ARN)
		if err != nil || (!strings.HasPrefix(parsedARN.Resource, "role/") && !strings.HasPrefix(parsedARN.Resource, "user/")) {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("identityMappings").Index(i).Child("arn"), mapping.ARN,
				"arn must be a valid IAM Role or User ARN such as arn:aws:iam::123456789012:role/KopsExampleRole"))
		}
	}
	return allErrs
}

func awsValidateAdditionalRoutes(fieldPath *field.Path, routes []kops.RouteSpec, networkCIDRs []*net.IPNet) field.ErrorList {
	allErrs := field.ErrorList{}

	for i, r := range routes {
		f := fieldPath.Index(i)

		// Check if target is a known type
		if !strings.HasPrefix(r.Target, "pcx-") &&
			!strings.HasPrefix(r.Target, "i-") &&
			!strings.HasPrefix(r.Target, "nat-") &&
			!strings.HasPrefix(r.Target, "tgw-") &&
			!strings.HasPrefix(r.Target, "igw-") &&
			!strings.HasPrefix(r.Target, "eigw-") {
			allErrs = append(allErrs, field.Invalid(f.Child("target"), r, "unknown target type for route"))
		}

		routeCIDR, errs := parseCIDR(f.Child("cidr"), r.CIDR)
		allErrs = append(allErrs, errs...)
		if routeCIDR != nil {
			for _, clusterNet := range networkCIDRs {
				if clusterNet.Contains(routeCIDR.IP) && strings.HasPrefix(r.Target, "pcx-") {
					allErrs = append(allErrs, field.Forbidden(f.Child("target"), "target is more specific than a network CIDR block. This route can target only an interface or an instance."))
				}
			}
		}
	}

	// Check for duplicated CIDR
	{
		cidrs := sets.NewString()
		for _, cidr := range networkCIDRs {
			cidrs.Insert(cidr.String())
		}
		for i := range routes {
			rCidr := routes[i].CIDR
			if cidrs.Has(rCidr) {
				allErrs = append(allErrs, field.Duplicate(fieldPath.Index(i).Child("cidr"), rCidr))
			}
			cidrs.Insert(rCidr)
		}
	}

	return allErrs
}
