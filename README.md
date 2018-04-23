# Lambda Function for Duo Log Collection

This is a Lambda function that pulls administration/authentication/telephony logs
from the Duo API and pushes them into a logging pipeline via a Kinesis stream.

## Basic operation

The function stores state information in an S3 bucket. The state information is essentially
timestamp data indicating when the last log message from the API was collected, so the
function knows when to begin requesting logs from for the next period. After the function runs
it updates the state data for the next iteration.

When log data is read, it is written to a Kinesis stream.

## Lambda Packaging

`make package` can be used to package the function in a zip file. A docker container is
temporarily used to generate the Linux executable and archive it in the zip.

## Deployment

Example deployment CloudFormation templates are available in the [misc](./misc)
directory. The example collects logs every 5 minutes, but a period of 15 minutes
is more suitable for production.

The `dev` template deploys roles and resources. The `dev-lambda` template deploys the
function and scheduling.

### Lambda Environment

The following settings are required for deployment.

#### DUOPULL_REGION

Should be set to the region the function's resources exist in.

#### DUOPULL_S3_BUCKET

The S3 bucket name the state should be saved and read from.

#### DUOPULL_KINESIS_STREAM

The Kinesis stream name the function writes events to.

#### DUOPULL_HOST

The Duo admin API host requests for logs will be made to.

#### DUOPULL_IKEY

The authentication identity to be used for API requests.

#### DUOPULL_SKEY

The secret key to be used for API requests.

## Development

### Environment

#### DEBUGAWS

If the DEBUGAWS environment variable is set to `1`, the function will generate mock events and make
requests to `https://www.mozilla.org` instead of the actual Duo API to confirm outbound
connectivity from the function. In this mode the function will update the S3 state and push
events to Kinesis but will not make requests to the actual Duo API.

#### DEBUGDUO

In this mode the function will not execute as a Lambda but will poll the Duo API for log data,
write the data to stdout and exit. The DEBUGDUO environment variable should be set to `1` to
enable this mode.
