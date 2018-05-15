package cluster

import (
	"bytes"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/kubernetes-incubator/kube-aws/cfnstack"
	"github.com/kubernetes-incubator/kube-aws/core/nodepool/config"
	"github.com/kubernetes-incubator/kube-aws/model"
	"github.com/kubernetes-incubator/kube-aws/plugin/clusterextension"
	"github.com/kubernetes-incubator/kube-aws/plugin/pluginmodel"
	"text/tabwriter"
)

const STACK_TEMPLATE_FILENAME = "stack.json"

type ClusterRef struct {
	config.ProvidedConfig
	session *session.Session
}

type Cluster struct {
	*ClusterRef
	*config.StackConfig
	assets cfnstack.Assets
}

type Info struct {
	Name string
}

type ec2DescribeKeyPairsService interface {
	DescribeKeyPairs(*ec2.DescribeKeyPairsInput) (*ec2.DescribeKeyPairsOutput, error)
}

type ec2CreateVolumeService interface {
	CreateVolume(*ec2.CreateVolumeInput) (*ec2.Volume, error)
}

func (c *Info) String() string {
	buf := new(bytes.Buffer)
	w := new(tabwriter.Writer)
	w.Init(buf, 0, 8, 0, '\t', 0)

	fmt.Fprintf(w, "Cluster Name:\t%s\n", c.Name)

	w.Flush()
	return buf.String()
}

func NewClusterRef(cfg *config.ProvidedConfig, awsDebug bool) *ClusterRef {
	awsConfig := aws.NewConfig().
		WithRegion(cfg.Region.String()).
		WithCredentialsChainVerboseErrors(true)

	if awsDebug {
		awsConfig = awsConfig.WithLogLevel(aws.LogDebug)
	}

	return &ClusterRef{
		ProvidedConfig: *cfg,
		session:        session.New(awsConfig),
	}
}

func NewCluster(provided *config.ProvidedConfig, opts config.StackTemplateOptions, plugins []*pluginmodel.Plugin, awsDebug bool) (*Cluster, error) {
	stackConfig, err := provided.StackConfig(opts)
	if err != nil {
		return nil, err
	}

	clusterRef := NewClusterRef(provided, awsDebug)

	c := &Cluster{
		StackConfig: stackConfig,
		ClusterRef:  clusterRef,
	}

	extras := clusterextension.NewExtrasFromPlugins(plugins, c.Plugins)

	extraStack, err := extras.NodePoolStack()
	if err != nil {
		return nil, fmt.Errorf("failed to load node pool stack extras from plugins: %v", err)
	}
	c.StackConfig.ExtraCfnResources = extraStack.Resources

	extraWorker, err := extras.Worker()
	if err != nil {
		return nil, fmt.Errorf("failed to load controller node extras from plugins: %v", err)
	}
	c.StackConfig.CustomSystemdUnits = append(c.StackConfig.CustomSystemdUnits, extraWorker.SystemdUnits...)
	c.StackConfig.CustomFiles = append(c.StackConfig.CustomFiles, extraWorker.Files...)
	c.StackConfig.IAMConfig.Policy.Statements = append(c.StackConfig.IAMConfig.Policy.Statements, extraWorker.IAMPolicyStatements...)

	for k, v := range extraWorker.NodeLabels {
		c.NodeSettings.NodeLabels[k] = v
	}
	for k, v := range extraWorker.FeatureGates {
		c.NodeSettings.FeatureGates[k] = v
	}

	c.assets, err = c.buildAssets()

	return c, err
}

func (c *Cluster) Assets() cfnstack.Assets {
	return c.assets
}

func (c *Cluster) buildAssets() (cfnstack.Assets, error) {
	var err error
	assets := cfnstack.NewAssetsBuilder(c.StackName(), c.StackConfig.S3URI, c.StackConfig.Region)
	if c.UserDataWorker, err = model.NewUserData(c.StackTemplateOptions.WorkerTmplFile, c.ComputedConfig); err != nil {
		return nil, fmt.Errorf("failed to render worker cloud config: %v", err)
	}

	if err = assets.AddUserDataPart(c.UserDataWorker, model.USERDATA_S3, "userdata-worker"); err != nil {
		return nil, fmt.Errorf("failed to render worker cloud config: %v", err)
	}

	stackTemplate, err := c.RenderStackTemplateAsString()
	if err != nil {
		return nil, fmt.Errorf("Error while rendering template : %v", err)
	}
	assets.Add(STACK_TEMPLATE_FILENAME, stackTemplate)

	return assets.Build(), nil
}

func (c *Cluster) TemplateURL() (string, error) {
	assets := c.Assets()
	asset, err := assets.FindAssetByStackAndFileName(c.StackName(), STACK_TEMPLATE_FILENAME)
	if err != nil {
		return "", fmt.Errorf("failed to get template url: %v", err)
	}
	return asset.URL()
}

func (c *Cluster) stackProvisioner() *cfnstack.Provisioner {
	stackPolicyBody := `{
  "Statement" : [
    {
       "Effect" : "Allow",
       "Principal" : "*",
       "Action" : "Update:*",
       "Resource" : "*"
     }
  ]
}`

	return cfnstack.NewProvisioner(c.StackName(), c.WorkerDeploymentSettings().StackTags(), c.S3URI, c.Region, stackPolicyBody, c.session(), c.CloudFormation.RoleARN)
}

func (c *Cluster) session() *session.Session {
	return c.ClusterRef.session
}

// ValidateStack validates the CloudFormation stack for this worker node pool already uploaded to S3
func (c *Cluster) ValidateStack() (string, error) {
	ec2Svc := ec2.New(c.session())
	if err := c.validateWorkerRootVolume(ec2Svc); err != nil {
		return "", err
	}
	if c.KeyName != "" {
		if err := c.validateKeyPair(ec2Svc); err != nil {
			return "", err
		}
	}

	stackTemplateURL, err := c.TemplateURL()
	if err != nil {
		return "", fmt.Errorf("failed to get template url : %v", err)
	}
	return c.stackProvisioner().ValidateStackAtURL(stackTemplateURL)
}

func (c *ClusterRef) validateKeyPair(ec2Svc ec2DescribeKeyPairsService) error {
	_, err := ec2Svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		KeyNames: []*string{aws.String(c.KeyName)},
	})

	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == "InvalidKeyPair.NotFound" {
				return fmt.Errorf("Key %s does not exist.", c.KeyName)
			}
		}
		return err
	}
	return nil
}

func (c *ClusterRef) validateWorkerRootVolume(ec2Svc ec2CreateVolumeService) error {

	//Send a dry-run request to validate the worker root volume parameters
	workerRootVolume := c.getWorkerRootVolumeConfig()
	if _, err := ec2Svc.CreateVolume(workerRootVolume); err != nil {
		operr, ok := err.(awserr.Error)

		if !ok || (ok && operr.Code() != "DryRunOperation") {
			return fmt.Errorf("create volume dry-run request failed: %v", err)
		}
	}

	return nil
}

func (c *ClusterRef) getWorkerRootVolumeConfig() *ec2.CreateVolumeInput {
	var workerRootVolume = &ec2.CreateVolumeInput{}

	switch c.RootVolume.Type {
	case "standard":
		workerRootVolume = &ec2.CreateVolumeInput{
			DryRun:           aws.Bool(true),
			AvailabilityZone: aws.String(c.Subnets[0].AvailabilityZone),
			Size:             aws.Int64(int64(c.RootVolume.Size)),
			VolumeType:       aws.String(c.RootVolume.Type),
		}
	case "io1":
		workerRootVolume = &ec2.CreateVolumeInput{
			DryRun:           aws.Bool(true),
			AvailabilityZone: aws.String(c.Subnets[0].AvailabilityZone),
			Iops:             aws.Int64(int64(c.RootVolume.IOPS)),
			Size:             aws.Int64(int64(c.RootVolume.Size)),
			VolumeType:       aws.String(c.RootVolume.Type),
		}
	case "gp2":
		workerRootVolume = &ec2.CreateVolumeInput{
			DryRun:           aws.Bool(true),
			AvailabilityZone: aws.String(c.Subnets[0].AvailabilityZone),
			Size:             aws.Int64(int64(c.RootVolume.Size)),
			VolumeType:       aws.String(c.RootVolume.Type),
		}
	default:
		workerRootVolume = &ec2.CreateVolumeInput{
			DryRun:           aws.Bool(true),
			AvailabilityZone: aws.String(c.Subnets[0].AvailabilityZone),
			Size:             aws.Int64(int64(c.RootVolume.Size)),
			VolumeType:       aws.String(c.RootVolume.Type),
		}
	}
	return workerRootVolume
}

func (c *ClusterRef) Info() (*Info, error) {
	var info Info
	{
		info.Name = c.NodePoolName
	}
	return &info, nil
}

func (c *ClusterRef) Destroy() error {
	return cfnstack.NewDestroyer(c.StackName(), c.session, c.CloudFormation.RoleARN).Destroy()
}
