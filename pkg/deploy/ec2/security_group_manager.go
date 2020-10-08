package ec2

import (
	"context"
	"errors"
	awssdk "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	ec2sdk "github.com/aws/aws-sdk-go/service/ec2"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/aws/services"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/deploy/tracking"
	ec2model "sigs.k8s.io/aws-load-balancer-controller/pkg/model/ec2"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/networking"
	"time"
)

const (
	defaultWaitSGDeletionPollInterval = 2 * time.Second
	defaultWaitSGDeletionTimeout      = 2 * time.Minute
)

// SecurityGroupManager is responsible for create/update/delete SecurityGroup resources.
type SecurityGroupManager interface {
	Create(ctx context.Context, resSG *ec2model.SecurityGroup) (ec2model.SecurityGroupStatus, error)

	Update(ctx context.Context, resSG *ec2model.SecurityGroup, sdkSG networking.SecurityGroupInfo) (ec2model.SecurityGroupStatus, error)

	Delete(ctx context.Context, sdkSG networking.SecurityGroupInfo) error
}

// NewDefaultSecurityGroupManager constructs new defaultSecurityGroupManager.
func NewDefaultSecurityGroupManager(ec2Client services.EC2, trackingProvider tracking.Provider, taggingManager TaggingManager,
	networkingSGReconciler networking.SecurityGroupReconciler, vpcID string, logger logr.Logger) *defaultSecurityGroupManager {
	return &defaultSecurityGroupManager{
		ec2Client:              ec2Client,
		trackingProvider:       trackingProvider,
		taggingManager:         taggingManager,
		networkingSGReconciler: networkingSGReconciler,
		vpcID:                  vpcID,
		logger:                 logger,

		waitSGDeletionPollInterval: defaultWaitSGDeletionPollInterval,
		waitSGDeletionTimeout:      defaultWaitSGDeletionTimeout,
	}
}

// default implementation for SecurityGroupManager.
type defaultSecurityGroupManager struct {
	ec2Client              services.EC2
	trackingProvider       tracking.Provider
	taggingManager         TaggingManager
	networkingSGReconciler networking.SecurityGroupReconciler
	vpcID                  string
	logger                 logr.Logger

	waitSGDeletionPollInterval time.Duration
	waitSGDeletionTimeout      time.Duration
}

func (m *defaultSecurityGroupManager) Create(ctx context.Context, resSG *ec2model.SecurityGroup) (ec2model.SecurityGroupStatus, error) {
	sgTags := m.trackingProvider.ResourceTags(resSG.Stack(), resSG, resSG.Spec.Tags)
	sdkTags := convertTagsToSDKTags(sgTags)
	permissionInfos, err := buildIPPermissionInfos(resSG.Spec.Ingress)
	if err != nil {
		return ec2model.SecurityGroupStatus{}, err
	}

	req := &ec2sdk.CreateSecurityGroupInput{
		VpcId:       awssdk.String(m.vpcID),
		GroupName:   awssdk.String(resSG.Spec.GroupName),
		Description: awssdk.String(resSG.Spec.Description),
		TagSpecifications: []*ec2sdk.TagSpecification{
			{
				ResourceType: awssdk.String("security-group"),
				Tags:         sdkTags,
			},
		},
	}
	m.logger.Info("creating securityGroup",
		"resourceID", resSG.ID())
	resp, err := m.ec2Client.CreateSecurityGroupWithContext(ctx, req)
	if err != nil {
		return ec2model.SecurityGroupStatus{}, err
	}
	sgID := awssdk.StringValue(resp.GroupId)
	m.logger.Info("created securityGroup",
		"resourceID", resSG.ID(),
		"securityGroupID", sgID)

	if err := m.networkingSGReconciler.ReconcileIngress(ctx, sgID, permissionInfos); err != nil {
		return ec2model.SecurityGroupStatus{}, err
	}

	return ec2model.SecurityGroupStatus{
		GroupID: sgID,
	}, nil
}

func (m *defaultSecurityGroupManager) Update(ctx context.Context, resSG *ec2model.SecurityGroup, sdkSG networking.SecurityGroupInfo) (ec2model.SecurityGroupStatus, error) {
	permissionInfos, err := buildIPPermissionInfos(resSG.Spec.Ingress)
	if err != nil {
		return ec2model.SecurityGroupStatus{}, err
	}
	if err := m.updateSDKSecurityGroupGroupWithTags(ctx, resSG, sdkSG); err != nil {
		return ec2model.SecurityGroupStatus{}, err
	}
	if err := m.networkingSGReconciler.ReconcileIngress(ctx, sdkSG.SecurityGroupID, permissionInfos); err != nil {
		return ec2model.SecurityGroupStatus{}, err
	}
	return ec2model.SecurityGroupStatus{
		GroupID: sdkSG.SecurityGroupID,
	}, nil
}

func (m *defaultSecurityGroupManager) Delete(ctx context.Context, sdkSG networking.SecurityGroupInfo) error {
	req := &ec2sdk.DeleteSecurityGroupInput{
		GroupId: awssdk.String(sdkSG.SecurityGroupID),
	}

	ctx, cancel := context.WithTimeout(ctx, m.waitSGDeletionTimeout)
	defer cancel()
	m.logger.Info("deleting securityGroup",
		"securityGroupID", sdkSG.SecurityGroupID)
	if err := wait.PollImmediateUntil(m.waitSGDeletionPollInterval, func() (done bool, err error) {
		if _, err := m.ec2Client.DeleteSecurityGroupWithContext(ctx, req); err != nil {
			if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == "DependencyViolation" {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}, ctx.Done()); err != nil {
		return err
	}
	m.logger.Info("deleted securityGroup",
		"securityGroupID", sdkSG.SecurityGroupID)

	return nil
}

func (m *defaultSecurityGroupManager) updateSDKSecurityGroupGroupWithTags(ctx context.Context, resSG *ec2model.SecurityGroup, sdkSG networking.SecurityGroupInfo) error {
	desiredSGTags := m.trackingProvider.ResourceTags(resSG.Stack(), resSG, resSG.Spec.Tags)
	return m.taggingManager.ReconcileTags(ctx, sdkSG.SecurityGroupID, desiredSGTags,
		WithCurrentTags(sdkSG.Tags),
		WithIgnoredTagKeys(m.trackingProvider.LegacyTagKeys()))
}

func buildIPPermissionInfos(permissions []ec2model.IPPermission) ([]networking.IPPermissionInfo, error) {
	permissionInfos := make([]networking.IPPermissionInfo, 0, len(permissions))
	for _, permission := range permissions {
		permissionInfo, err := buildIPPermissionInfo(permission)
		if err != nil {
			return nil, err
		}
		permissionInfos = append(permissionInfos, permissionInfo)
	}
	return permissionInfos, nil
}

func buildIPPermissionInfo(permission ec2model.IPPermission) (networking.IPPermissionInfo, error) {
	protocol := permission.IPProtocol
	if len(permission.IPRanges) == 1 {
		labels := networking.NewIPPermissionLabelsForRawDescription(permission.IPRanges[0].Description)
		return networking.NewCIDRIPPermission(protocol, permission.FromPort, permission.ToPort, permission.IPRanges[0].CIDRIP, labels), nil
	}
	if len(permission.IPv6Range) == 1 {
		labels := networking.NewIPPermissionLabelsForRawDescription(permission.IPv6Range[0].Description)
		return networking.NewCIDRv6IPPermission(protocol, permission.FromPort, permission.ToPort, permission.IPv6Range[0].CIDRIPv6, labels), nil
	}
	if len(permission.UserIDGroupPairs) == 1 {
		labels := networking.NewIPPermissionLabelsForRawDescription(permission.UserIDGroupPairs[0].Description)
		return networking.NewGroupIDIPPermission(protocol, permission.FromPort, permission.ToPort, permission.UserIDGroupPairs[0].GroupID, labels), nil
	}
	return networking.IPPermissionInfo{}, errors.New("invalid ipPermission")
}
