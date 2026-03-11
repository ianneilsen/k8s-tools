#!/usr/bin/env bash
# aws_ecr_cleanup.sh
# WARNING: This script DELETES ecr images on aws if you have uploaded your own built images!


# WARNING WARNING: Use only if you really want a complete fresh start.

set -euo pipefail

Delete ECR repos
 # Define variables #
CRYSTAL_ECR_REPO=$(jq < cfn-output.json -r '.CrystalEcrRepo' | cut -d'/' -f2)
NODEJS_ECR_REPO=$(jq < cfn-output.json -r '.NodeJSEcrRepo' | cut -d'/' -f2)
# Delete ecr images #
aws ecr list-images \
  --repository-name $CRYSTAL_ECR_REPO | \
jq -r ' .imageIds[] | [ .imageDigest ] | @tsv ' | \
  while IFS=$'\t' read -r imageDigest; do 
    aws ecr batch-delete-image \
      --repository-name $CRYSTAL_ECR_REPO \
      --image-ids imageDigest=$imageDigest
  done
aws ecr list-images \
  --repository-name $NODEJS_ECR_REPO | \
jq -r ' .imageIds[] | [ .imageDigest ] | @tsv ' | \
  while IFS=$'\t' read -r imageDigest; do 
    aws ecr batch-delete-image \
      --repository-name $NODEJS_ECR_REPO \
      --image-ids imageDigest=$imageDigest
  done

# refs:
# https://www.appmeshworkshop.com/cleanup/ecr/
