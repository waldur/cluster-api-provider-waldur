/*
Copyright 2026.

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

package controller

import (
	"context"

	uuid "github.com/google/uuid"
	openapitypes "github.com/oapi-codegen/runtime/types"
	"github.com/pkg/errors"
	infrastructurev1beta2 "github.com/sergei-zaiaev/cluster-api-provider-waldur/api/v1beta2"
	waldurclient "github.com/waldur/go-client"
)

// getFlavor looks up an OpenStack flavor by name within an offering.
func getFlavor(ctx context.Context, waldur waldurclient.ClientWithResponses, offering *waldurclient.PublicOfferingDetails, flavorName string) (*waldurclient.OpenStackFlavor, error) {
	resp, err := waldur.OpenstackFlavorsListWithResponse(ctx, &waldurclient.OpenstackFlavorsListParams{
		OfferingUuid: offering.Uuid,
		NameExact:    &flavorName,
	})
	if err != nil {
		return nil, err
	}
	flavors := *resp.JSON200
	if len(flavors) == 0 {
		return nil, errors.Errorf("flavor %q not found in offering %s", flavorName, *offering.Slug)
	}
	return &flavors[0], nil
}

// getImage looks up an OpenStack image by name within an offering.
func getImage(ctx context.Context, waldur waldurclient.ClientWithResponses, offering *waldurclient.PublicOfferingDetails, imageName string) (*waldurclient.OpenStackImage, error) {
	resp, err := waldur.OpenstackImagesListWithResponse(ctx, &waldurclient.OpenstackImagesListParams{
		OfferingUuid: offering.Uuid,
		NameExact:    &imageName,
	})
	if err != nil {
		return nil, err
	}
	images := *resp.JSON200
	if len(images) == 0 {
		return nil, errors.Errorf("image %q not found in offering %s", imageName, *offering.Slug)
	}
	return &images[0], nil
}

// getTenantSecurityGroups returns all security groups belonging to a tenant.
func getTenantSecurityGroups(ctx context.Context, waldur waldurclient.ClientWithResponses, tenantUuid uuid.UUID) ([]waldurclient.OpenStackSecurityGroup, error) {
	resp, err := waldur.OpenstackSecurityGroupsListWithResponse(ctx, &waldurclient.OpenstackSecurityGroupsListParams{
		TenantUuid: &tenantUuid,
	})
	if err != nil {
		return nil, errors.Wrap(err, "unable to list security groups")
	}
	return *resp.JSON200, nil
}

// getTenantSubnets returns all subnets belonging to a tenant.
func getTenantSubnets(ctx context.Context, waldur waldurclient.ClientWithResponses, tenantUuid uuid.UUID) ([]waldurclient.OpenStackSubNet, error) {
	resp, err := waldur.OpenstackSubnetsListWithResponse(ctx, &waldurclient.OpenstackSubnetsListParams{
		TenantUuid: &tenantUuid,
	})
	if err != nil {
		return nil, errors.Wrap(err, "unable to list subnets")
	}
	return *resp.JSON200, nil
}

// getTenantVolumeTypes returns all volume types available in a tenant.
func getTenantVolumeTypes(ctx context.Context, waldur waldurclient.ClientWithResponses, tenantUuid uuid.UUID) ([]waldurclient.OpenStackVolumeType, error) {
	resp, err := waldur.OpenstackVolumeTypesListWithResponse(ctx, &waldurclient.OpenstackVolumeTypesListParams{
		TenantUuid: &tenantUuid,
	})
	if err != nil {
		return nil, errors.Wrap(err, "unable to list volume types")
	}
	return *resp.JSON200, nil
}

// getOffering looks up a Waldur marketplace offering by slug.
func getOffering(ctx context.Context, waldur waldurclient.ClientWithResponses, offeringSlug string) (*waldurclient.PublicOfferingDetails, error) {
	offeringResponse, err := waldur.MarketplacePublicOfferingsListWithResponse(ctx, &waldurclient.MarketplacePublicOfferingsListParams{
		Slug: &offeringSlug,
	})
	if err != nil {
		return nil, err
	}

	offerings := *offeringResponse.JSON200
	if len(offerings) == 0 {
		return nil, errors.Errorf("unable to find an offering with slug %s", offeringSlug)
	}

	return &offerings[0], nil
}

// getOffering looks up a Waldur marketplace offering by UUID.
func getOfferingByUUID(ctx context.Context, waldur waldurclient.ClientWithResponses, offeringUuid openapitypes.UUID) (*waldurclient.PublicOfferingDetails, error) {
	offeringResponse, err := waldur.MarketplacePublicOfferingsRetrieveWithResponse(ctx, offeringUuid, &waldurclient.MarketplacePublicOfferingsRetrieveParams{})
	if err != nil {
		return nil, err
	}

	offering := *offeringResponse.JSON200

	return &offering, nil
}

// getMarketplaceResource fetches a Waldur marketplace resource by its UUID string.
func getMarketplaceResource(ctx context.Context, waldur waldurclient.ClientWithResponses, marketplaceResourceUuid string) (*waldurclient.Resource, error) {
	resourceUuid, err := uuid.Parse(marketplaceResourceUuid)
	if err != nil {
		return nil, errors.Wrap(err, "unable to parse marketplace resource UUID")
	}

	resp, err := waldur.MarketplaceResourcesRetrieveWithResponse(ctx, resourceUuid, &waldurclient.MarketplaceResourcesRetrieveParams{})
	if err != nil {
		return nil, errors.Wrap(err, "unable to retrieve marketplace resource")
	}

	return resp.JSON200, nil
}

// refreshOrder fetches the current state of a Waldur order and updates the in-memory record.
func refreshOrder(ctx context.Context, waldur waldurclient.ClientWithResponses, existingOrder *infrastructurev1beta2.WaldurOrder) error {
	orderUuid, err := uuid.Parse(existingOrder.Uuid)
	if err != nil {
		return err
	}

	orderResponse, err := waldur.MarketplaceOrdersRetrieveWithResponse(ctx, orderUuid, &waldurclient.MarketplaceOrdersRetrieveParams{})
	if err != nil {
		return err
	}

	order := orderResponse.JSON200
	existingOrder.State = *order.State

	if order.ResourceUuid != nil {
		tenantUuid := order.ResourceUuid.String()
		existingOrder.ResourceUuid = &tenantUuid
	}

	return nil
}

// submitTerminationOrder submits a Waldur marketplace termination order for the given
// marketplace resource UUID and returns the resulting WaldurOrder.
func submitTerminationOrder(ctx context.Context, waldur waldurclient.ClientWithResponses, marketplaceResourceUuid string) (*infrastructurev1beta2.WaldurOrder, error) {
	resourceUuid, err := uuid.Parse(marketplaceResourceUuid)
	if err != nil {
		return nil, errors.Wrap(err, "unable to parse marketplace resource UUID")
	}

	resp, err := waldur.MarketplaceResourcesTerminateWithResponse(ctx, resourceUuid, waldurclient.ResourceTerminateRequest{})
	if err != nil {
		return nil, err
	}

	if resp.StatusCode() != 200 && resp.StatusCode() != 202 {
		return nil, errors.Errorf("unable to submit termination order, status %d: %s", resp.StatusCode(), string(resp.Body))
	}

	if resp.JSON200 == nil || resp.JSON200.OrderUuid == nil {
		return nil, errors.New("termination order response missing order UUID")
	}

	return &infrastructurev1beta2.WaldurOrder{
		Uuid:                    resp.JSON200.OrderUuid.String(),
		Type:                    waldurclient.Terminate,
		State:                   waldurclient.OrderStatePendingProvider,
		MarketplaceResourceUuid: marketplaceResourceUuid,
	}, nil
}

// isOrderTerminal reports whether an order has reached a terminal state.
func isOrderTerminal(state waldurclient.OrderState) bool {
	return state == waldurclient.OrderStateDone ||
		state == waldurclient.OrderStateErred ||
		state == waldurclient.OrderStateCanceled
}
