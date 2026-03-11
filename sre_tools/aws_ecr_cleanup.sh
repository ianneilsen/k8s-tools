AWS EKS scripts
================

NOTE: always set set -euxo pipefail in your bash script


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

refs:
https://www.appmeshworkshop.com/cleanup/ecr/
