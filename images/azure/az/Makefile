# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

AZ_SUBSCRIPTION_ID=$(shell az account show --query id -o tsv)
ROLE_NAME=Packer Image Builder
SP_NAME=Packer-Golden-Image-Builder
BUILD_RG=rg-packer-build-temp
IMAGE_RG=rg-clickhouse-images

# Generate temp role definition with the current subscription ID
generate-role-definition:
	@echo "Generating role definition with subscription ID: ${AZ_SUBSCRIPTION_ID}"
	@sed 's|<YOUR_SUBSCRIPTION_ID>|${AZ_SUBSCRIPTION_ID}|g' packer-role.json > packer-role-temp.json

# Check if role already exists
check-role:
	@az role definition list --name "${ROLE_NAME}" --query "[].name" -o tsv 2>/dev/null || echo "Role does not exist"

# Delete existing role (if needed to recreate)
delete-role:
	az role definition delete --name "${ROLE_NAME}" || true

# Create the custom role for Packer
create-role: generate-role-definition
	@echo "Creating custom role: ${ROLE_NAME}"
	az role definition create --role-definition packer-role-temp.json
	@rm -f packer-role-temp.json

# Update existing role
update-role: generate-role-definition
	@echo "Updating custom role: ${ROLE_NAME}"
	az role definition update --role-definition packer-role-temp.json
	@rm -f packer-role-temp.json

# Create the service principal with the custom role
create-sp:
	@echo "Creating service principal: ${SP_NAME}"
	az ad sp create-for-rbac --name "${SP_NAME}" \
		--role "${ROLE_NAME}" \
		--scopes "/subscriptions/${AZ_SUBSCRIPTION_ID}"

# Full setup: create role and service principal
setup-packer-auth: create-role create-sp
	@echo "Packer authentication setup complete"

init-azure:
	# Create the Resource Groups
	az group create -n ${BUILD_RG} -l eastus
	az group create -n ${IMAGE_RG} -l eastus

	# Create the Service Principal with limited scope
	# Note: We assign it 'Contributor' ONLY on these two RGs
	az ad sp create-for-rbac --name "${SP_NAME}" \
		--role Contributor \
		--scopes "/subscriptions/${AZ_SUBSCRIPTION_ID}/resourceGroups/${BUILD_RG}" \
		         "/subscriptions/${AZ_SUBSCRIPTION_ID}/resourceGroups/${IMAGE_RG}"

# Clean up generated files
clean:
	rm -f packer-role-temp.json

.PHONY: generate-role-definition check-role delete-role create-role update-role create-sp setup-packer-auth init-azure clean
