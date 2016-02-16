// +build !lambdabinary

//go:generate rm -rf ./resources/provision/node_modules
//go:generate npm install ./resources/provision/ --prefix ./resources/provision
// There's a handful of subdirectories that we don't need at runtime...
//go:generate rm -rf ./resources/provision/node_modules/aws-sdk/dist/
//go:generate rm -rf ./resources/provision/node_modules/aws-sdk/dist-tools/
// Zip up the modules
//go:generate bash -c "pushd ./resources/provision; zip -vr ./node_modules.zip ./node_modules/"
//go:generate rm -rf ./resources/provision/node_modules

// Embed the custom service handlers
// TODO: Once AWS lambda supports golang as first class, move the
// NodeJS custom action helpers into golang
//go:generate go run ./vendor/github.com/mjibson/esc/main.go -o ./CONSTANTS.go -private -pkg sparta ./resources
//go:generate go run ./resources/awsbinary/insertTags.go ./CONSTANTS !lambdabinary

// Create a secondary CONSTANTS_AWSBINARY.go file with empty content.  The next step will insert the
// build tags at the head of each file so that they are mutually exclusive, similar to the
// lambdabinaryshims.go file
//go:generate go run ./vendor/github.com/mjibson/esc/main.go -o ./CONSTANTS_AWSBINARY.go -private -pkg sparta ./resources/awsbinary/README.md
//go:generate go run ./resources/awsbinary/insertTags.go ./CONSTANTS_AWSBINARY lambdabinary

// cleanup
//go:generate rm -f ./resources/provision/node_modules.zip

package sparta

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	gocf "github.com/crewjam/go-cloudformation"
)

////////////////////////////////////////////////////////////////////////////////
// CONSTANTS
////////////////////////////////////////////////////////////////////////////////

const (
	// OutputSpartaHomeKey is the keyname used in the CloudFormation Output
	// that stores the Sparta home URL.
	// @enum OutputKey
	OutputSpartaHomeKey = "SpartaHome"

	// OutputSpartaVersionKey is the keyname used in the CloudFormation Output
	// that stores the Sparta version used to provision/update the service.
	// @enum OutputKey
	OutputSpartaVersionKey = "SpartaVersion"
)

// The basename of the scripts that are embedded into CONSTANTS.go
// by `esc` during the generate phase.  In order to export these, there
// MUST be a corresponding PROXIED_MODULES entry for the base filename
// in resources/index.js
var customResourceScripts = []string{"cfn-response.js",
	"sparta_utils.js",
	"apigateway.js",
	"events.js",
	"logs.js",
	"s3.js",
	"ses.js",
	"sns.js",
	"s3Site.js",
	"golang-constants.json",
	"apigateway/inputmapping_default.vtl",
	"apigateway/inputmapping_json.vtl"}

// The relative path of the custom scripts that is used
// to create the filename relative path when creating the custom archive
const provisioningResourcesRelPath = "/resources/provision"

// Represents data associated with provisioning the S3 Site iff defined
type s3SiteContext struct {
	s3Site             *S3Site
	s3SiteLambdaZipKey string
}

// Rollback function called in the event of a stack provisioning failure
type rollbackFunction func(logger *logrus.Logger) error

// Type of a workflow step.  Each step is responsible
// for returning the next step or an error if the overall
// workflow should stop.
type workflowStep func(ctx *workflowContext) (workflowStep, error)

////////////////////////////////////////////////////////////////////////////////
// Workflow context
// The workflow context is created by `provision` and provided to all
// functions that constitute the provisioning workflow.
type workflowContext struct {
	// Is this is a -dry-run?
	noop bool
	// Canonical basename of the service.  Also used as the CloudFormation
	// stack name
	serviceName string
	// Service description
	serviceDescription string
	// The slice of Lambda functions that constitute the service
	lambdaAWSInfos []*LambdaAWSInfo
	// Optional APIGateway definition to associate with this service
	api *API
	// Optional S3 site data to provision together with this service
	s3SiteContext *s3SiteContext
	// CloudFormation Template
	cfTemplate *gocf.Template
	// Cached IAM role name map.  Used to support dynamic and static IAM role
	// names.  Static ARN role names are checked for existence via AWS APIs
	// prior to CloudFormation provisioning.
	lambdaIAMRoleNameMap map[string]*gocf.StringExpr
	// The user-supplied S3 bucket where service artifacts should be posted.
	s3Bucket string
	// The programmatically determined S3 item key for this service's cloudformation
	// definition.
	s3LambdaZipKey string
	// AWS Session to be used for all API calls made in the process of provisioning
	// this service.
	awsSession *session.Session
	// IO writer for autogenerated template results
	templateWriter io.Writer
	// Preconfigured logger
	logger *logrus.Logger
	// Optional rollback functions that workflow steps may append to if they
	// have made mutations during provisioning.
	rollbackFunctions []rollbackFunction
}

// Register a rollback function in the event that the provisioning
// function failed.
func (ctx *workflowContext) registerRollback(userFunction rollbackFunction) {
	if nil == ctx.rollbackFunctions || len(ctx.rollbackFunctions) <= 0 {
		ctx.rollbackFunctions = make([]rollbackFunction, 0)
	}
	ctx.rollbackFunctions = append(ctx.rollbackFunctions, userFunction)
}

// Run any provided rollback functions
func (ctx *workflowContext) rollback() {
	// Run each cleanup function concurrently.  If there's an error
	// all we're going to do is log it as a warning, since at this
	// point there's nothing to do...
	var wg sync.WaitGroup
	wg.Add(len(ctx.rollbackFunctions))

	ctx.logger.WithFields(logrus.Fields{
		"RollbackCount": len(ctx.rollbackFunctions),
	}).Info("Invoking rollback functions")

	for _, eachCleanup := range ctx.rollbackFunctions {
		go func(cleanupFunc rollbackFunction, goLogger *logrus.Logger) {
			// Decrement the counter when the goroutine completes.
			defer wg.Done()
			// Fetch the URL.
			err := cleanupFunc(goLogger)
			if nil != err {
				ctx.logger.WithFields(logrus.Fields{
					"Error": err,
				}).Warning("Failed to cleanup resource")
			}
		}(eachCleanup, ctx.logger)
	}
	wg.Wait()
}

////////////////////////////////////////////////////////////////////////////////
// Private - START
//

// Create a temporary file in the current working directory
func temporaryFile(name string) (*os.File, error) {
	workingDir, err := os.Getwd()
	if nil != err {
		return nil, err
	}
	tmpFile, err := ioutil.TempFile(workingDir, name)
	if err != nil {
		return nil, errors.New("Failed to create temporary file")
	}
	return tmpFile, nil
}

// Create a source object (either a file, or a directory that will be recursively
// added) to a previously opened zip.Writer.
func addToZip(zipWriter *zip.Writer, source string, rootSource string, logger *logrus.Logger) error {
	fullPathSource, err := filepath.Abs(source)
	if nil != err {
		return err
	}
	appendFile := func(sourceFile string) error {
		// Get the relative path
		var name = filepath.Base(sourceFile)
		if sourceFile != rootSource {
			name = strings.TrimPrefix(strings.TrimPrefix(sourceFile, rootSource), string(os.PathSeparator))
		}
		binaryWriter, err := zipWriter.Create(name)
		if err != nil {
			return fmt.Errorf("Failed to create ZIP entry: %s", filepath.Base(sourceFile))
		}
		reader, err := os.Open(sourceFile)
		if err != nil {
			return fmt.Errorf("Failed to open file: %s", sourceFile)
		}
		defer reader.Close()
		io.Copy(binaryWriter, reader)
		logger.WithFields(logrus.Fields{
			"Path": sourceFile,
		}).Debug("Archiving file")

		return nil
	}

	directoryWalker := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = strings.TrimPrefix(strings.TrimPrefix(path, rootSource), string(os.PathSeparator))
		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}
		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(writer, file)
		return err
	}

	fileInfo, err := os.Stat(fullPathSource)
	if nil != err {
		return err
	}
	switch mode := fileInfo.Mode(); {
	case mode.IsDir():
		err = filepath.Walk(fullPathSource, directoryWalker)
	case mode.IsRegular():
		err = appendFile(fullPathSource)
	default:
		err = errors.New("Inavlid source type")
	}
	zipWriter.Close()
	return err
}

// Ensure that the S3 bucket we're using for archives has an object expiration policy.  The
// uploaded archives otherwise will be orphaned in S3 since the template can't manage the
// associated resources
func ensureExpirationPolicy(awsSession *session.Session, S3Bucket string, noop bool, logger *logrus.Logger) error {
	if noop {
		logger.WithFields(logrus.Fields{
			"BucketName": S3Bucket,
		}).Info("Bypassing bucket expiration policy check due to -n/-noop command line argument")
		return nil
	}
	s3Svc := s3.New(awsSession)
	params := &s3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String(S3Bucket), // Required
	}
	showWarning := false
	resp, err := s3Svc.GetBucketLifecycleConfiguration(params)
	if nil != err {
		showWarning = strings.Contains(err.Error(), "NoSuchLifecycleConfiguration")
		if !showWarning {
			return fmt.Errorf("Failed to fetch S3 Bucket Policy: %s", err.Error())
		}
	} else {
		for _, eachRule := range resp.Rules {
			if *eachRule.Status == s3.ExpirationStatusEnabled {
				showWarning = false
			}
		}
	}
	if showWarning {
		logger.WithFields(logrus.Fields{
			"Bucket":    S3Bucket,
			"Reference": "http://docs.aws.amazon.com/AmazonS3/latest/dev/how-to-set-lifecycle-configuration-intro.html",
		}).Warning("Bucket should have ObjectExpiration lifecycle enabled.")
	} else {
		logger.WithFields(logrus.Fields{
			"Bucket": S3Bucket,
			"Rules":  resp.Rules,
		}).Debug("Bucket lifecycle configuration")
	}
	return nil
}

// Upload a local file to S3.  Returns the s3 keyname of the
// uploaded item, or an error
func uploadLocalFileToS3(packagePath string, awsSession *session.Session, S3Bucket string, noop bool, logger *logrus.Logger) (string, error) {
	// Query the S3 bucket for the bucket policies.  The bucket _should_ have ObjectExpiration,
	// otherwise we're just going to orphan our binaries...
	err := ensureExpirationPolicy(awsSession, S3Bucket, noop, logger)
	if nil != err {
		return "", fmt.Errorf("Failed to ensure bucket policies: %s", err.Error())
	}
	// Then do the actual work
	reader, err := os.Open(packagePath)
	if nil != err {
		return "", fmt.Errorf("Failed to open local archive for S3 upload: %s", err.Error())
	}
	defer func() {
		reader.Close()
		err = os.Remove(packagePath)
		if nil != err {
			logger.WithFields(logrus.Fields{
				"Path":  packagePath,
				"Error": err,
			}).Warn("Failed to delete local file")
		}
	}()

	// Cache it in case there was an error & we need to cleanup
	keyName := filepath.Base(packagePath)

	uploadInput := &s3manager.UploadInput{
		Bucket:      &S3Bucket,
		Key:         &keyName,
		ContentType: aws.String("application/zip"),
		Body:        reader,
	}

	if noop {
		logger.WithFields(logrus.Fields{
			"Bucket": S3Bucket,
			"Key":    keyName,
		}).Info("Bypassing S3 ZIP upload due to -n/-noop command line argument")
	} else {
		logger.WithFields(logrus.Fields{
			"Source": packagePath,
		}).Info("Uploading local file to S3")
		uploader := s3manager.NewUploader(awsSession)
		result, err := uploader.Upload(uploadInput)
		if nil != err {
			return "", err
		}
		logger.WithFields(logrus.Fields{

			"URL": result.Location,
		}).Info("Upload complete")
	}
	return keyName, nil
}

// Creates an S3 rollback function that attempts to delete a previously
// uploaded item.
func createS3RollbackFunc(awsSession *session.Session, s3Bucket string, s3Key string, noop bool) rollbackFunction {
	return func(logger *logrus.Logger) error {
		if !noop {
			logger.Info("Attempting to cleanup S3 item: ", s3Key)
			s3Client := s3.New(awsSession)
			params := &s3.DeleteObjectInput{
				Bucket: aws.String(s3Bucket),
				Key:    aws.String(s3Key),
			}
			_, err := s3Client.DeleteObject(params)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"Error": err,
				}).Warn("Failed to delete S3 item during rollback cleanup")
			} else {
				logger.WithFields(logrus.Fields{
					"Bucket": s3Bucket,
					"Key":    s3Key,
				}).Debug("Item deleted during rollback cleanup")
			}
			return err
		}
		logger.WithFields(logrus.Fields{
			"S3Bucket": s3Bucket,
			"S3Key":    s3Key,
		}).Info("Bypassing rollback cleanup ")
		return nil
	}
}

// Private - END
////////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////////
// Workflow steps
////////////////////////////////////////////////////////////////////////////////

// Verify & cache the IAM rolename to ARN mapping
func verifyIAMRoles(ctx *workflowContext) (workflowStep, error) {
	// The map is either a literal Arn from a pre-existing role name
	// or a gocf.RefFunc() value.
	// Don't verify them, just create them...
	ctx.logger.Info("Verifying IAM Lambda execution roles")
	ctx.lambdaIAMRoleNameMap = make(map[string]*gocf.StringExpr, 0)
	svc := iam.New(ctx.awsSession)

	for _, eachLambda := range ctx.lambdaAWSInfos {
		if "" != eachLambda.RoleName && nil != eachLambda.RoleDefinition {
			return nil, fmt.Errorf("Both RoleName and RoleDefinition defined for lambda: %s", eachLambda.lambdaFnName)
		}

		// Get the IAM role name
		if "" != eachLambda.RoleName {
			_, exists := ctx.lambdaIAMRoleNameMap[eachLambda.RoleName]
			if !exists {
				// Check the role
				params := &iam.GetRoleInput{
					RoleName: aws.String(eachLambda.RoleName),
				}
				ctx.logger.Debug("Checking IAM RoleName: ", eachLambda.RoleName)
				resp, err := svc.GetRole(params)
				if err != nil {
					ctx.logger.Error(err.Error())
					return nil, err
				}
				// Cache it - we'll need it later when we create the
				// CloudFormation template which needs the execution Arn (not role)
				ctx.lambdaIAMRoleNameMap[eachLambda.RoleName] = gocf.String(*resp.Role.Arn)
			}
		} else {
			logicalName := eachLambda.RoleDefinition.logicalName()
			_, exists := ctx.lambdaIAMRoleNameMap[logicalName]
			if !exists {
				// Insert it into the resource creation map and add
				// the "Ref" entry to the hashmap
				ctx.cfTemplate.AddResource(logicalName,
					eachLambda.RoleDefinition.toResource(eachLambda.EventSourceMappings, ctx.logger))

				ctx.lambdaIAMRoleNameMap[logicalName] = gocf.GetAtt(logicalName, "Arn")
			}
		}
	}
	ctx.logger.WithFields(logrus.Fields{
		"Count": len(ctx.lambdaIAMRoleNameMap),
	}).Info("IAM roles verified")

	return createPackageStep(), nil
}

// Return a string representation of a JS function call that can be exposed
// to AWS Lambda
func createNewNodeJSProxyEntry(lambdaInfo *LambdaAWSInfo, logger *logrus.Logger) string {
	logger.WithFields(logrus.Fields{
		"FunctionName": lambdaInfo.lambdaFnName,
	}).Info("Registering Sparta function")

	// We do know the CF resource name here - could write this into
	// index.js and expose a GET localhost:9000/lambdaMetadata
	// which wraps up DescribeStackResource for the running
	// lambda function
	primaryEntry := fmt.Sprintf("exports[\"%s\"] = createForwarder(\"/%s\");\n",
		lambdaInfo.jsHandlerName(),
		lambdaInfo.lambdaFnName)
	return primaryEntry
}

// Return the StackEvents for the given StackName/StackID
func stackEvents(stackID string, cfService *cloudformation.CloudFormation) ([]*cloudformation.StackEvent, error) {
	var events []*cloudformation.StackEvent

	nextToken := ""
	for {
		params := &cloudformation.DescribeStackEventsInput{
			StackName: aws.String(stackID),
		}
		if len(nextToken) > 0 {
			params.NextToken = aws.String(nextToken)
		}

		resp, err := cfService.DescribeStackEvents(params)
		if nil != err {
			return nil, err
		}
		events = append(events, resp.StackEvents...)
		if nil == resp.NextToken {
			break
		} else {
			nextToken = *resp.NextToken
		}
	}
	return events, nil
}

// Build and package the application
func createPackageStep() workflowStep {

	return func(ctx *workflowContext) (workflowStep, error) {
		// Compile the source to linux...
		sanitizedServiceName := sanitizedName(ctx.serviceName)
		executableOutput := fmt.Sprintf("%s.lambda.amd64", sanitizedServiceName)
		cmd := exec.Command("go", "build", "-o", executableOutput, "-tags", "lambdabinary", ".")

		ctx.logger.WithFields(logrus.Fields{
			"Arguments": cmd.Args,
		}).Debug("Building application binary")

		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH=amd64")
		ctx.logger.WithFields(logrus.Fields{
			"Name": executableOutput,
		}).Info("Compiling binary")

		ctx.logger.WithFields(logrus.Fields{
			"Env": cmd.Env,
		}).Debug("Compilation environment")

		outputWriter := ctx.logger.Writer()
		defer outputWriter.Close()
		cmd.Stdout = outputWriter
		cmd.Stderr = outputWriter

		err := cmd.Run()
		if err != nil {
			return nil, err
		}
		defer func() {
			err := os.Remove(executableOutput)
			if nil != err {
				ctx.logger.WithFields(logrus.Fields{
					"File":  executableOutput,
					"Error": err,
				}).Warn("Failed to delete binary")
			}
		}()

		// Binary size
		stat, err := os.Stat(executableOutput)
		if err != nil {
			return nil, errors.New("Failed to stat build output")
		}

		ctx.logger.WithFields(logrus.Fields{
			"KB": stat.Size() / 1024,
			"MB": stat.Size() / (1024 * 1024),
		}).Info("Executable binary size")

		tmpFile, err := temporaryFile(sanitizedServiceName)
		if err != nil {
			return nil, errors.New("Failed to create temporary file")
		}
		defer func() {
			tmpFile.Close()
		}()

		ctx.logger.WithFields(logrus.Fields{
			"TempName": tmpFile.Name(),
		}).Info("Creating ZIP archive for upload")

		lambdaArchive := zip.NewWriter(tmpFile)
		defer lambdaArchive.Close()

		// File info for the binary executable
		binaryWriter, err := lambdaArchive.Create(filepath.Base(executableOutput))
		if err != nil {
			return nil, fmt.Errorf("Failed to create ZIP entry: %s", filepath.Base(executableOutput))
		}
		reader, err := os.Open(executableOutput)
		if err != nil {
			return nil, fmt.Errorf("Failed to open file: %s", executableOutput)
		}
		defer reader.Close()
		io.Copy(binaryWriter, reader)

		// Add the string literal adapter, which requires us to add exported
		// functions to the end of index.js.  These NodeJS exports will be
		// linked to the AWS Lambda NodeJS function name, and are basically
		// automatically generated pass through proxies to the golang HTTP handler.
		nodeJSWriter, err := lambdaArchive.Create("index.js")
		if err != nil {
			return nil, errors.New("Failed to create ZIP entry: index.js")
		}
		nodeJSSource := _escFSMustString(false, "/resources/index.js")
		nodeJSSource += "\n// DO NOT EDIT - CONTENT UNTIL EOF IS AUTOMATICALLY GENERATED\n"
		for _, eachLambda := range ctx.lambdaAWSInfos {
			nodeJSSource += createNewNodeJSProxyEntry(eachLambda, ctx.logger)
		}
		// Finally, replace
		// 	SPARTA_BINARY_NAME = 'Sparta.lambda.amd64';
		// with the service binary name
		nodeJSSource += fmt.Sprintf("SPARTA_BINARY_NAME='%s';\n", executableOutput)
		// And the service name
		nodeJSSource += fmt.Sprintf("SPARTA_SERVICE_NAME='%s';\n", ctx.serviceName)
		ctx.logger.WithFields(logrus.Fields{
			"index.js": nodeJSSource,
		}).Debug("Dynamically generated NodeJS adapter")

		stringReader := strings.NewReader(nodeJSSource)
		io.Copy(nodeJSWriter, stringReader)

		// Next embed the custom resource scripts into the package.
		// TODO - conditionally include custom NodeJS scripts based on service requirement
		ctx.logger.Debug("Embedding CustomResource scripts")

		for _, eachName := range customResourceScripts {
			resourceName := fmt.Sprintf("%s/%s", provisioningResourcesRelPath, eachName)
			resourceContent := _escFSMustString(false, resourceName)
			stringReader := strings.NewReader(resourceContent)
			embedWriter, err := lambdaArchive.Create(eachName)
			if nil != err {
				return nil, err
			}
			ctx.logger.WithFields(logrus.Fields{
				"Name": eachName,
			}).Debug("Script name")

			io.Copy(embedWriter, stringReader)
		}

		// And finally, if there is a node_modules.zip file, then include it.  The
		// node_modules archive includes supplementary libraries that the
		// CustomResource handlers may need at CloudFormation stack creation time.
		nodeModulesZipRelName := fmt.Sprintf("%s/node_modules.zip", provisioningResourcesRelPath)
		nodeModuleBytes, err := _escFSByte(false, nodeModulesZipRelName)
		if nil == err {
			nodeModuleReader, err := zip.NewReader(bytes.NewReader(nodeModuleBytes), int64(len(nodeModuleBytes)))
			if err != nil {
				return nil, err
			}
			ctx.logger.WithFields(logrus.Fields{
				"Name": nodeModulesZipRelName,
			}).Debug("Embedding CustomResource node_modules.zip")

			for _, zipFile := range nodeModuleReader.File {
				embedWriter, err := lambdaArchive.Create(zipFile.Name)
				if nil != err {
					return nil, err
				}
				ctx.logger.WithFields(logrus.Fields{
					"Name": zipFile.Name,
				}).Debug("Copying embedded node_module file")

				sourceReader, err := zipFile.Open()
				if err != nil {
					return nil, err
				}
				io.Copy(embedWriter, sourceReader)
			}
		} else {
			ctx.logger.WithFields(logrus.Fields{
				"Name":  nodeModulesZipRelName,
				"Error": err,
			}).Warn("Failed to load node_modules.zip for embedding")
		}
		return createUploadStep(tmpFile.Name()), nil
	}
}

// Given the zipped binary in packagePath, upload the primary code bundle
// and optional S3 site resources iff they're defined.
func createUploadStep(packagePath string) workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		var uploadErrors []error
		var wg sync.WaitGroup

		// We always need to upload the primary binary
		wg.Add(1)
		go func() {
			defer wg.Done()
			keyName, err := uploadLocalFileToS3(packagePath,
				ctx.awsSession,
				ctx.s3Bucket,
				ctx.noop,
				ctx.logger)
			ctx.s3LambdaZipKey = keyName

			if nil != err {
				uploadErrors = append(uploadErrors, err)
			} else {
				ctx.registerRollback(createS3RollbackFunc(ctx.awsSession, ctx.s3Bucket, ctx.s3LambdaZipKey, ctx.noop))
			}
		}()

		// S3 site to compress & upload
		if nil != ctx.s3SiteContext.s3Site {
			wg.Add(1)
			go func() {
				defer wg.Done()

				tempName := fmt.Sprintf("%s-S3Site", ctx.serviceName)
				tmpFile, err := temporaryFile(tempName)
				if err != nil {
					uploadErrors = append(uploadErrors,
						errors.New("Failed to create temporary S3 site archive file"))
					return
				}

				// Add the contents to the Zip file
				zipArchive := zip.NewWriter(tmpFile)
				absResourcePath, err := filepath.Abs(ctx.s3SiteContext.s3Site.resources)
				if nil != err {
					uploadErrors = append(uploadErrors, err)
					return
				}

				ctx.logger.WithFields(logrus.Fields{
					"S3Key":  path.Base(tmpFile.Name()),
					"Source": absResourcePath,
				}).Info("Creating S3Site archive")

				err = addToZip(zipArchive, absResourcePath, absResourcePath, ctx.logger)
				if nil != err {
					uploadErrors = append(uploadErrors, err)
					return
				}

				zipArchive.Close()
				tmpFile.Close()
				// Upload it & save the key
				s3SiteLambdaZipKey, err := uploadLocalFileToS3(tmpFile.Name(), ctx.awsSession, ctx.s3Bucket, ctx.noop, ctx.logger)
				ctx.s3SiteContext.s3SiteLambdaZipKey = s3SiteLambdaZipKey
				if nil != err {
					uploadErrors = append(uploadErrors, err)
				} else {
					ctx.registerRollback(createS3RollbackFunc(ctx.awsSession, ctx.s3Bucket, ctx.s3SiteContext.s3SiteLambdaZipKey, ctx.noop))
				}
			}()
		}
		wg.Wait()

		if len(uploadErrors) > 0 {
			errorText := "Encountered multiple errors during upload:\n"
			for _, eachError := range uploadErrors {
				errorText += fmt.Sprintf("%s%s\n", errorText, eachError.Error())
				return nil, errors.New(errorText)
			}
		}
		return ensureCloudFormationStack(), nil
	}
}

// Does a given stack exist?
func stackExists(stackNameOrID string, cf *cloudformation.CloudFormation, logger *logrus.Logger) (bool, error) {
	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackNameOrID),
	}
	describeStacksOutput, err := cf.DescribeStacks(describeStacksInput)
	logger.WithFields(logrus.Fields{
		"DescribeStackOutput": describeStacksOutput,
	}).Debug("DescribeStackOutput results")

	exists := false
	if err != nil {
		logger.WithFields(logrus.Fields{
			"DescribeStackOutputError": err,
		}).Debug("DescribeStackOutput")

		// If the stack doesn't exist, then no worries
		if strings.Contains(err.Error(), "does not exist") {
			exists = false
		} else {
			return false, err
		}
	} else {
		exists = true
	}
	return exists, nil
}

func stackCapabilities(template *gocf.Template) []*string {
	// Only require IAM capability if the definition requires it.
	var capabilities []*string
	for _, eachResource := range template.Resources {
		if eachResource.Properties.ResourceType() == "AWS::IAM::Role" {
			found := false
			for _, eachElement := range capabilities {
				found = (found || (*eachElement == "CAPABILITY_IAM"))
			}
			if !found {
				capabilities = append(capabilities, aws.String("CAPABILITY_IAM"))
			}
		}
	}
	return capabilities
}

// Converge the stack to the new state, taking into account whether
// it was previously provisioned.
func convergeStackState(cfTemplateURL string, ctx *workflowContext) (*cloudformation.Stack, error) {
	awsCloudFormation := cloudformation.New(ctx.awsSession)

	// Does it exist?
	exists, err := stackExists(ctx.serviceName, awsCloudFormation, ctx.logger)
	if nil != err {
		return nil, err
	}
	stackID := ""
	if exists {
		// Update stack
		updateStackInput := &cloudformation.UpdateStackInput{
			StackName:    aws.String(ctx.serviceName),
			TemplateURL:  aws.String(cfTemplateURL),
			Capabilities: stackCapabilities(ctx.cfTemplate),
		}
		updateStackResponse, err := awsCloudFormation.UpdateStack(updateStackInput)
		if nil != err {
			return nil, err
		}

		ctx.logger.WithFields(logrus.Fields{
			"StackID": *updateStackResponse.StackId,
		}).Info("Issued stack update request")

		stackID = *updateStackResponse.StackId
	} else {
		// Create stack
		createStackInput := &cloudformation.CreateStackInput{
			StackName:        aws.String(ctx.serviceName),
			TemplateURL:      aws.String(cfTemplateURL),
			TimeoutInMinutes: aws.Int64(5),
			OnFailure:        aws.String(cloudformation.OnFailureDelete),
			Capabilities:     stackCapabilities(ctx.cfTemplate),
		}
		createStackResponse, err := awsCloudFormation.CreateStack(createStackInput)
		if nil != err {
			return nil, err
		}

		ctx.logger.WithFields(logrus.Fields{
			"StackID": *createStackResponse.StackId,
		}).Info("Creating stack")

		stackID = *createStackResponse.StackId
	}

	// Poll for the current stackID state, and
	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackID),
	}

	var stackInfo *cloudformation.Stack
	var convegeStackStateSucceeded bool
	for waitComplete := false; !waitComplete; {
		sleepDuration := time.Duration(11+rand.Int31n(13)) * time.Second
		time.Sleep(sleepDuration)

		describeStacksOutput, err := awsCloudFormation.DescribeStacks(describeStacksInput)
		if nil != err {
			// TODO - add retry iff we're RateExceeded due to collective access
			return nil, err
		}
		if len(describeStacksOutput.Stacks) <= 0 {
			return nil, fmt.Errorf("Failed to enumerate stack info: %v", *describeStacksInput.StackName)
		}
		stackInfo = describeStacksOutput.Stacks[0]
		switch *stackInfo.StackStatus {
		case cloudformation.StackStatusCreateComplete,
			cloudformation.StackStatusUpdateComplete:
			convegeStackStateSucceeded = true
			waitComplete = true
		case
			// Include DeleteComplete as new provisions will automatically rollback
			cloudformation.StackStatusDeleteComplete,
			cloudformation.StackStatusCreateFailed,
			cloudformation.StackStatusDeleteFailed,
			cloudformation.StackStatusRollbackFailed,
			cloudformation.StackStatusRollbackComplete:
			convegeStackStateSucceeded = false
			waitComplete = true
		default:
			if exists {
				ctx.logger.Info("Waiting for UpdateStack to complete")
			} else {
				ctx.logger.Info("Waiting for CreateStack to complete")
			}
		}
	}

	// If it didn't work, then output some failure information
	if !convegeStackStateSucceeded {
		// Get the stack events and find the ones that failed.
		events, err := stackEvents(stackID, awsCloudFormation)
		if nil != err {
			return nil, err
		}

		ctx.logger.Error("Stack provisioning error")
		for _, eachEvent := range events {
			switch *eachEvent.ResourceStatus {
			case cloudformation.ResourceStatusCreateFailed,
				cloudformation.ResourceStatusDeleteFailed,
				cloudformation.ResourceStatusUpdateFailed:
				errMsg := fmt.Sprintf("\tError ensuring %s (%s): %s",
					*eachEvent.ResourceType,
					*eachEvent.LogicalResourceId,
					*eachEvent.ResourceStatusReason)
				ctx.logger.Error(errMsg)
			default:
				// NOP
			}
		}
		return nil, fmt.Errorf("Failed to provision: %s", ctx.serviceName)
	} else if nil != stackInfo.Outputs {
		for _, eachOutput := range stackInfo.Outputs {
			ctx.logger.WithFields(logrus.Fields{
				"Key":         *eachOutput.OutputKey,
				"Value":       *eachOutput.OutputValue,
				"Description": *eachOutput.Description,
			}).Info("Stack output")
		}
	}
	return stackInfo, nil
}

func ensureCloudFormationStack() workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		for _, eachEntry := range ctx.lambdaAWSInfos {
			err := eachEntry.export(ctx.serviceName,
				ctx.s3Bucket,
				ctx.s3LambdaZipKey,
				ctx.lambdaIAMRoleNameMap,
				ctx.cfTemplate,
				ctx.logger)
			if nil != err {
				return nil, err
			}
		}
		// If there's an API gateway definition, include the resources that provision it. Since this export will likely
		// generate outputs that the s3 site needs, we'll use a temporary outputs accumulator, pass that to the S3Site
		// if it's defined, and then merge it with the normal output map.
		apiGatewayTemplate := gocf.NewTemplate()

		if nil != ctx.api {
			err := ctx.api.export(ctx.s3Bucket,
				ctx.s3LambdaZipKey,
				ctx.lambdaIAMRoleNameMap,
				apiGatewayTemplate,
				ctx.logger)
			if nil == err {
				err = safeMergeTemplates(apiGatewayTemplate, ctx.cfTemplate, ctx.logger)
			}
			if nil != err {
				return nil, fmt.Errorf("Failed to export APIGateway template resources")
			}
		}
		// If there's a Site defined, include the resources the provision it
		if nil != ctx.s3SiteContext.s3Site {
			ctx.s3SiteContext.s3Site.export(ctx.s3Bucket,
				ctx.s3LambdaZipKey,
				ctx.s3SiteContext.s3SiteLambdaZipKey,
				apiGatewayTemplate.Outputs,
				ctx.lambdaIAMRoleNameMap,
				ctx.cfTemplate,
				ctx.logger)
		}

		// Save the output
		ctx.cfTemplate.Outputs[OutputSpartaHomeKey] = &gocf.Output{
			Description: "Sparta Home",
			Value:       gocf.String("http://gosparta.io"),
		}
		ctx.cfTemplate.Outputs[OutputSpartaVersionKey] = &gocf.Output{
			Description: "Sparta Version",
			Value:       gocf.String(SpartaVersion),
		}

		// Next pass - exchange outputs between dependencies.  Lambda functions
		for _, eachResource := range ctx.cfTemplate.Resources {
			// Only apply this to lambda functions
			if eachResource.Properties.ResourceType() == "AWS::Lambda::Function" {
				// Update the metdata with a reference to the output of each
				// depended on item...
				for _, eachDependsKey := range eachResource.DependsOn {
					dependencyOutputs, _ := outputsForResource(ctx.cfTemplate, eachDependsKey, ctx.logger)
					if nil != dependencyOutputs && len(dependencyOutputs) != 0 {
						ctx.logger.WithFields(logrus.Fields{
							"Resource":  eachDependsKey,
							"DependsOn": eachResource.DependsOn,
							"Outputs":   dependencyOutputs,
						}).Debug("Resource metadata")
						safeMetadataInsert(eachResource, eachDependsKey, dependencyOutputs)
					}
				}
				// Also include standard AWS outputs at a resource level if a lambda
				// needs to self-discover other resources
				safeMetadataInsert(eachResource, TagStackRegion, gocf.Ref("AWS::Region"))
				safeMetadataInsert(eachResource, TagStackID, gocf.Ref("AWS::StackId"))
				safeMetadataInsert(eachResource, TagStackName, gocf.Ref("AWS::StackName"))
			}
		}

		// Generate a complete CloudFormation template
		cfTemplate, err := json.Marshal(ctx.cfTemplate)
		if err != nil {
			ctx.logger.Error("Failed to Marshal CloudFormation template: ", err.Error())
			return nil, err
		}

		// Upload the actual CloudFormation template to S3 to increase the template
		// size limit
		contentBody := string(cfTemplate)
		sanitizedServiceName := sanitizedName(ctx.serviceName)
		hash := sha1.New()
		hash.Write([]byte(contentBody))
		s3keyName := fmt.Sprintf("%s-%s-cf.json", sanitizedServiceName, hex.EncodeToString(hash.Sum(nil)))

		uploadInput := &s3manager.UploadInput{
			Bucket:      &ctx.s3Bucket,
			Key:         &s3keyName,
			ContentType: aws.String("application/json"),
			Body:        strings.NewReader(contentBody),
		}
		formatted, err := json.MarshalIndent(contentBody, "", " ")
		if nil != err {
			return nil, err
		}

		ctx.logger.WithFields(logrus.Fields{
			"Body": string(formatted),
		}).Debug("CloudFormation template body")

		if nil != ctx.templateWriter {
			io.WriteString(ctx.templateWriter, string(formatted))
		}

		if ctx.noop {
			ctx.logger.WithFields(logrus.Fields{
				"Bucket": ctx.s3Bucket,
				"Key":    s3keyName,
			}).Info("Bypassing template upload & creation due to -n/-noop command line argument")
		} else {
			ctx.logger.Info("Uploading CloudFormation template")
			uploader := s3manager.NewUploader(ctx.awsSession)
			templateUploadResult, err := uploader.Upload(uploadInput)
			if nil != err {
				return nil, err
			}
			// Cleanup if there's a problem
			ctx.registerRollback(createS3RollbackFunc(ctx.awsSession, ctx.s3Bucket, s3keyName, ctx.noop))

			// Be transparent
			ctx.logger.WithFields(logrus.Fields{
				"URL": templateUploadResult.Location,
			}).Info("Template uploaded")

			stack, err := convergeStackState(templateUploadResult.Location, ctx)
			if nil != err {
				return nil, err
			}
			ctx.logger.WithFields(logrus.Fields{
				"StackName":    *stack.StackName,
				"StackId":      *stack.StackId,
				"CreationTime": *stack.CreationTime,
			}).Info("Stack provisioned")
		}
		return nil, nil
	}
}

// Provision compiles, packages, and provisions (either via create or update) a Sparta application.
// The serviceName is the service's logical
// identify and is used to determine create vs update operations.  The compilation options/flags are:
//
// 	TAGS:         -tags lambdabinary
// 	ENVIRONMENT:  GOOS=linux GOARCH=amd64 GO15VENDOREXPERIMENT=1
//
// The compiled binary is packaged with a NodeJS proxy shim to manage AWS Lambda setup & invocation per
// http://docs.aws.amazon.com/lambda/latest/dg/authoring-function-in-nodejs.html
//
// The two files are ZIP'd, posted to S3 and used as an input to a dynamically generated CloudFormation
// template (http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/Welcome.html)
// which creates or updates the service state.
//
// More information on golang 1.5's support for vendor'd resources is documented at
//
//  https://docs.google.com/document/d/1Bz5-UB7g2uPBdOx-rw5t9MxJwkfpx90cqG9AFL0JAYo/edit
//  https://medium.com/@freeformz/go-1-5-s-vendor-experiment-fd3e830f52c3#.voiicue1j
//
// type Configuration struct {
//     Val   string
//     Proxy struct {
//         Address string
//         Port    string
//     }
// }
func Provision(noop bool,
	serviceName string,
	serviceDescription string,
	lambdaAWSInfos []*LambdaAWSInfo,
	api *API,
	site *S3Site,
	s3Bucket string,
	templateWriter io.Writer,
	logger *logrus.Logger) error {

	startTime := time.Now()

	ctx := &workflowContext{
		noop:               noop,
		serviceName:        serviceName,
		serviceDescription: serviceDescription,
		lambdaAWSInfos:     lambdaAWSInfos,
		api:                api,
		s3SiteContext: &s3SiteContext{
			s3Site: site,
		},
		cfTemplate:     gocf.NewTemplate(),
		s3Bucket:       s3Bucket,
		awsSession:     awsSession(logger),
		templateWriter: templateWriter,
		logger:         logger,
	}
	ctx.cfTemplate.Description = serviceDescription

	if len(lambdaAWSInfos) <= 0 {
		return errors.New("No lambda functions provided to Sparta.Provision()")
	}

	// Start the workflow
	for step := verifyIAMRoles; step != nil; {
		next, err := step(ctx)
		if err != nil {
			ctx.rollback()
			return err
		}
		if next == nil {
			elapsed := time.Since(startTime)
			ctx.logger.WithFields(logrus.Fields{
				"Seconds": fmt.Sprintf("%.f", elapsed.Seconds()),
			}).Info("Elapsed time")
			break
		} else {
			step = next
		}
	}
	return nil
}
