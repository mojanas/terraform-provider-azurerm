package cognitive

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/hashicorp/go-azure-helpers/lang/response"
	"github.com/hashicorp/go-azure-helpers/resourcemanager/commonschema"
	"github.com/hashicorp/go-azure-helpers/resourcemanager/identity"
	"github.com/hashicorp/go-azure-helpers/resourcemanager/location"
	"github.com/hashicorp/terraform-provider-azurerm/helpers/azure"
	"github.com/hashicorp/terraform-provider-azurerm/helpers/tf"
	commonValidate "github.com/hashicorp/terraform-provider-azurerm/helpers/validate"
	"github.com/hashicorp/terraform-provider-azurerm/internal/clients"
	legacyIdentity "github.com/hashicorp/terraform-provider-azurerm/internal/identity"
	"github.com/hashicorp/terraform-provider-azurerm/internal/locks"
	"github.com/hashicorp/terraform-provider-azurerm/internal/services/cognitive/parse"
	"github.com/hashicorp/terraform-provider-azurerm/internal/services/cognitive/sdk/2021-04-30/cognitiveservicesaccounts"
	"github.com/hashicorp/terraform-provider-azurerm/internal/services/cognitive/validate"
	"github.com/hashicorp/terraform-provider-azurerm/internal/services/network"
	networkParse "github.com/hashicorp/terraform-provider-azurerm/internal/services/network/parse"
	searchValidate "github.com/hashicorp/terraform-provider-azurerm/internal/services/search/validate"
	storageValidate "github.com/hashicorp/terraform-provider-azurerm/internal/services/storage/validate"
	"github.com/hashicorp/terraform-provider-azurerm/internal/tags"
	"github.com/hashicorp/terraform-provider-azurerm/internal/tf/pluginsdk"
	"github.com/hashicorp/terraform-provider-azurerm/internal/tf/set"
	"github.com/hashicorp/terraform-provider-azurerm/internal/tf/validation"
	"github.com/hashicorp/terraform-provider-azurerm/internal/timeouts"
	"github.com/hashicorp/terraform-provider-azurerm/utils"
)

func resourceCognitiveAccount() *pluginsdk.Resource {
	return &pluginsdk.Resource{
		Create: resourceCognitiveAccountCreate,
		Read:   resourceCognitiveAccountRead,
		Update: resourceCognitiveAccountUpdate,
		Delete: resourceCognitiveAccountDelete,

		Timeouts: &pluginsdk.ResourceTimeout{
			Create: pluginsdk.DefaultTimeout(30 * time.Minute),
			Read:   pluginsdk.DefaultTimeout(5 * time.Minute),
			Update: pluginsdk.DefaultTimeout(30 * time.Minute),
			Delete: pluginsdk.DefaultTimeout(30 * time.Minute),
		},

		Importer: pluginsdk.ImporterValidatingResourceId(func(id string) error {
			_, err := parse.AccountID(id)
			return err
		}),

		Schema: resourceCognitiveAccountSchema(),
	}
}

func resourceCognitiveAccountCreate(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Cognitive.AccountsClient
	subscriptionId := meta.(*clients.Client).Account.SubscriptionId
	ctx, cancel := timeouts.ForCreate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	kind := d.Get("kind").(string)

	id := cognitiveservicesaccounts.NewAccountID(subscriptionId, d.Get("resource_group_name").(string), d.Get("name").(string))
	if d.IsNewResource() {
		existing, err := client.AccountsGet(ctx, id)
		if err != nil {
			if !response.WasNotFound(existing.HttpResponse) {
				return fmt.Errorf("checking for presence of existing %s: %+v", id, err)
			}
		}

		if !response.WasNotFound(existing.HttpResponse) {
			return tf.ImportAsExistsError("azurerm_cognitive_account", id.ID())
		}
	}

	sku, err := expandAccountSkuName(d.Get("sku_name").(string))
	if err != nil {
		return fmt.Errorf("expanding sku_name for %s: %v", id, err)
	}

	networkAcls, subnetIds := expandCognitiveAccountNetworkAcls(d)

	// also lock on the Virtual Network ID's since modifications in the networking stack are exclusive
	virtualNetworkNames := make([]string, 0)
	for _, v := range subnetIds {
		id, err := networkParse.SubnetIDInsensitively(v)
		if err != nil {
			return err
		}
		if !utils.SliceContainsValue(virtualNetworkNames, id.VirtualNetworkName) {
			virtualNetworkNames = append(virtualNetworkNames, id.VirtualNetworkName)
		}
	}

	locks.MultipleByName(&virtualNetworkNames, network.VirtualNetworkResourceName)
	defer locks.UnlockMultipleByName(&virtualNetworkNames, network.VirtualNetworkResourceName)

	publicNetworkAccess := cognitiveservicesaccounts.PublicNetworkAccessEnabled
	if !d.Get("public_network_access_enabled").(bool) {
		publicNetworkAccess = cognitiveservicesaccounts.PublicNetworkAccessDisabled
	}

	apiProps, err := expandCognitiveAccountAPIProperties(d)
	if err != nil {
		return err
	}

	props := cognitiveservicesaccounts.Account{
		Kind:     utils.String(kind),
		Location: utils.String(azure.NormalizeLocation(d.Get("location").(string))),
		Sku:      sku,
		Properties: &cognitiveservicesaccounts.AccountProperties{
			ApiProperties:                 apiProps,
			NetworkAcls:                   networkAcls,
			CustomSubDomainName:           utils.String(d.Get("custom_subdomain_name").(string)),
			AllowedFqdnList:               utils.ExpandStringSlice(d.Get("fqdns").([]interface{})),
			PublicNetworkAccess:           &publicNetworkAccess,
			UserOwnedStorage:              expandCognitiveAccountStorage(d.Get("storage").([]interface{})),
			RestrictOutboundNetworkAccess: utils.Bool(d.Get("outbound_network_access_restricted").(bool)),
			DisableLocalAuth:              utils.Bool(!d.Get("local_auth_enabled").(bool)),
		},
		Tags: expandTags(d.Get("tags").(map[string]interface{})),
	}

	identity, err := expandCognitiveAccountIdentity(d.Get("identity").([]interface{}))
	if err != nil {
		return fmt.Errorf("expanding `identity`: %+v", err)
	}
	props.Identity = identity

	if _, err := client.AccountsCreate(ctx, id, props); err != nil {
		return fmt.Errorf("creating %s: %+v", id, err)
	}

	stateConf := &pluginsdk.StateChangeConf{
		Pending:    []string{"Accepted", "Creating"},
		Target:     []string{"Succeeded"},
		Refresh:    cognitiveAccountStateRefreshFunc(ctx, client, id),
		MinTimeout: 15 * time.Second,
		Timeout:    d.Timeout(pluginsdk.TimeoutCreate),
	}

	if _, err = stateConf.WaitForStateContext(ctx); err != nil {
		return fmt.Errorf("waiting for creation of %s: %+v", id, err)
	}

	d.SetId(id.ID())
	return resourceCognitiveAccountRead(d, meta)
}

func resourceCognitiveAccountUpdate(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Cognitive.AccountsClient
	ctx, cancel := timeouts.ForUpdate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := cognitiveservicesaccounts.ParseAccountID(d.Id())
	if err != nil {
		return err
	}

	sku, err := expandAccountSkuName(d.Get("sku_name").(string))
	if err != nil {
		return fmt.Errorf("expanding sku_name for %s: %+v", *id, err)
	}

	networkAcls, subnetIds := expandCognitiveAccountNetworkAcls(d)

	// also lock on the Virtual Network ID's since modifications in the networking stack are exclusive
	virtualNetworkNames := make([]string, 0)
	for _, v := range subnetIds {
		id, err := networkParse.SubnetIDInsensitively(v)
		if err != nil {
			return err
		}
		if !utils.SliceContainsValue(virtualNetworkNames, id.VirtualNetworkName) {
			virtualNetworkNames = append(virtualNetworkNames, id.VirtualNetworkName)
		}
	}

	locks.MultipleByName(&virtualNetworkNames, network.VirtualNetworkResourceName)
	defer locks.UnlockMultipleByName(&virtualNetworkNames, network.VirtualNetworkResourceName)

	publicNetworkAccess := cognitiveservicesaccounts.PublicNetworkAccessEnabled
	if !d.Get("public_network_access_enabled").(bool) {
		publicNetworkAccess = cognitiveservicesaccounts.PublicNetworkAccessDisabled
	}

	apiProps, err := expandCognitiveAccountAPIProperties(d)
	if err != nil {
		return err
	}

	props := cognitiveservicesaccounts.Account{
		Sku: sku,
		Properties: &cognitiveservicesaccounts.AccountProperties{
			ApiProperties:                 apiProps,
			NetworkAcls:                   networkAcls,
			CustomSubDomainName:           utils.String(d.Get("custom_subdomain_name").(string)),
			AllowedFqdnList:               utils.ExpandStringSlice(d.Get("fqdns").([]interface{})),
			PublicNetworkAccess:           &publicNetworkAccess,
			UserOwnedStorage:              expandCognitiveAccountStorage(d.Get("storage").([]interface{})),
			RestrictOutboundNetworkAccess: utils.Bool(d.Get("outbound_network_access_restricted").(bool)),
			DisableLocalAuth:              utils.Bool(!d.Get("local_auth_enabled").(bool)),
		},
		Tags: expandTags(d.Get("tags").(map[string]interface{})),
	}
	identityRaw := d.Get("identity").([]interface{})
	identity, err := expandCognitiveAccountIdentity(identityRaw)
	if err != nil {
		return fmt.Errorf("expanding `identity`: %+v", err)
	}
	props.Identity = identity

	if _, err = client.AccountsUpdate(ctx, *id, props); err != nil {
		return fmt.Errorf("updating %s: %+v", *id, err)
	}

	stateConf := &pluginsdk.StateChangeConf{
		Pending:    []string{"Accepted"},
		Target:     []string{"Succeeded"},
		Refresh:    cognitiveAccountStateRefreshFunc(ctx, client, *id),
		MinTimeout: 15 * time.Second,
		Timeout:    d.Timeout(pluginsdk.TimeoutCreate),
	}

	if _, err = stateConf.WaitForStateContext(ctx); err != nil {
		return fmt.Errorf("waiting for update of %s: %+v", *id, err)
	}
	return resourceCognitiveAccountRead(d, meta)
}

func resourceCognitiveAccountRead(d *pluginsdk.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).Cognitive.AccountsClient
	ctx, cancel := timeouts.ForRead(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := cognitiveservicesaccounts.ParseAccountID(d.Id())
	if err != nil {
		return err
	}

	resp, err := client.AccountsGet(ctx, *id)
	if err != nil {
		if response.WasNotFound(resp.HttpResponse) {
			log.Printf("[DEBUG] %s was not found", *id)
			d.SetId("")
			return nil
		}

		return fmt.Errorf("retrieving %s: %+v", *id, err)
	}

	keys, err := client.AccountsListKeys(ctx, *id)
	if err != nil {
		// note for the resource we shouldn't gracefully fail since we have permission to CRUD it
		return fmt.Errorf("listing the Keys for %s: %+v", *id, err)
	}

	if model := keys.Model; model != nil {
		d.Set("primary_access_key", model.Key1)
		d.Set("secondary_access_key", model.Key2)
	}

	d.Set("name", id.AccountName)
	d.Set("resource_group_name", id.ResourceGroupName)

	if model := resp.Model; model != nil {
		d.Set("kind", model.Kind)

		d.Set("location", location.NormalizeNilable(model.Location))
		if sku := model.Sku; sku != nil {
			d.Set("sku_name", sku.Name)
		}

		identity, err := flattenCognitiveAccountIdentity(model.Identity)
		if err != nil {
			return err
		}
		d.Set("identity", identity)

		if props := model.Properties; props != nil {
			if apiProps := props.ApiProperties; apiProps != nil {
				d.Set("qna_runtime_endpoint", apiProps.QnaRuntimeEndpoint)
				d.Set("custom_question_answering_search_service_id", apiProps.QnaAzureSearchEndpointId)
				d.Set("metrics_advisor_aad_client_id", apiProps.AadClientId)
				d.Set("metrics_advisor_aad_tenant_id", apiProps.AadTenantId)
				d.Set("metrics_advisor_super_user_name", apiProps.SuperUser)
				d.Set("metrics_advisor_website_name", apiProps.WebsiteName)
			}
			d.Set("endpoint", props.Endpoint)
			d.Set("custom_subdomain_name", props.CustomSubDomainName)
			if err := d.Set("network_acls", flattenCognitiveAccountNetworkAcls(props.NetworkAcls)); err != nil {
				return fmt.Errorf("setting `network_acls` for Cognitive Account %q: %+v", id, err)
			}
			d.Set("fqdns", utils.FlattenStringSlice(props.AllowedFqdnList))

			publicNetworkAccess := true
			if props.PublicNetworkAccess != nil {
				publicNetworkAccess = *props.PublicNetworkAccess == cognitiveservicesaccounts.PublicNetworkAccessEnabled
			}
			d.Set("public_network_access_enabled", publicNetworkAccess)

			if err := d.Set("storage", flattenCognitiveAccountStorage(props.UserOwnedStorage)); err != nil {
				return fmt.Errorf("setting `storages` for Cognitive Account %q: %+v", id, err)
			}
			outboundNetworkAccessRestricted := false
			if props.RestrictOutboundNetworkAccess != nil {
				outboundNetworkAccessRestricted = *props.RestrictOutboundNetworkAccess
			}
			//lintignore:R001
			d.Set("outbound_network_access_restricted", outboundNetworkAccessRestricted)

			localAuthEnabled := true
			if props.DisableLocalAuth != nil {
				localAuthEnabled = !*props.DisableLocalAuth
			}
			d.Set("local_auth_enabled", localAuthEnabled)
		}

		return tags.FlattenAndSet(d, flattenTags(model.Tags))
	}
	return nil
}

func resourceCognitiveAccountDelete(d *pluginsdk.ResourceData, meta interface{}) error {
	accountsClient := meta.(*clients.Client).Cognitive.AccountsClient
	deletedAccountsClient := meta.(*clients.Client).Cognitive.AccountsClient
	ctx, cancel := timeouts.ForDelete(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := cognitiveservicesaccounts.ParseAccountID(d.Id())
	if err != nil {
		return err
	}

	// first we need to retrieve it, since we need the location to be able to purge it
	log.Printf("[DEBUG] Retrieving %s..", *id)
	account, err := accountsClient.AccountsGet(ctx, *id)
	if err != nil || account.Model == nil || account.Model.Location == nil {
		return fmt.Errorf("retrieving %s: %+v", *id, err)
	}

	deletedAccountId := cognitiveservicesaccounts.NewDeletedAccountID(id.SubscriptionId, *account.Model.Location, id.ResourceGroupName, id.AccountName)
	if err != nil {
		return err
	}

	log.Printf("[DEBUG] Deleting %s..", *id)
	if err := accountsClient.AccountsDeleteThenPoll(ctx, *id); err != nil {
		return fmt.Errorf("deleting %s: %+v", *id, err)
	}

	if meta.(*clients.Client).Features.CognitiveAccount.PurgeSoftDeleteOnDestroy {
		log.Printf("[DEBUG] Purging %s..", *id)
		if err := deletedAccountsClient.DeletedAccountsPurgeThenPoll(ctx, deletedAccountId); err != nil {
			return fmt.Errorf("purging %s: %+v", *id, err)
		}
	} else {
		log.Printf("[DEBUG] Skipping Purge of %s", *id)
	}

	return nil
}

func expandAccountSkuName(skuName string) (*cognitiveservicesaccounts.Sku, error) {
	var tier cognitiveservicesaccounts.SkuTier
	switch skuName[0:1] {
	case "F":
		tier = cognitiveservicesaccounts.SkuTierFree
	case "S":
		tier = cognitiveservicesaccounts.SkuTierStandard
	case "P":
		tier = cognitiveservicesaccounts.SkuTierPremium
	case "E":
		tier = cognitiveservicesaccounts.SkuTierEnterprise
	default:
		return nil, fmt.Errorf("sku_name %s has unknown sku tier %s", skuName, skuName[0:1])
	}

	return &cognitiveservicesaccounts.Sku{
		Name: skuName,
		Tier: &tier,
	}, nil
}

func cognitiveAccountStateRefreshFunc(ctx context.Context, client *cognitiveservicesaccounts.CognitiveServicesAccountsClient, id cognitiveservicesaccounts.AccountId) pluginsdk.StateRefreshFunc {
	return func() (interface{}, string, error) {
		res, err := client.AccountsGet(ctx, id)
		if err != nil {
			return nil, "", fmt.Errorf("polling for %s: %+v", id, err)
		}

		if res.Model != nil && res.Model.Properties != nil && res.Model.Properties.ProvisioningState != nil {
			return res, string(*res.Model.Properties.ProvisioningState), nil
		}
		return nil, "", fmt.Errorf("unable to read provisioning state")
	}
}

func expandCognitiveAccountNetworkAcls(d *pluginsdk.ResourceData) (*cognitiveservicesaccounts.NetworkRuleSet, []string) {
	input := d.Get("network_acls").([]interface{})
	subnetIds := make([]string, 0)
	if len(input) == 0 || input[0] == nil {
		return nil, subnetIds
	}

	v := input[0].(map[string]interface{})

	defaultAction := cognitiveservicesaccounts.NetworkRuleAction(v["default_action"].(string))

	ipRulesRaw := v["ip_rules"].(*pluginsdk.Set)
	ipRules := make([]cognitiveservicesaccounts.IpRule, 0)

	for _, v := range ipRulesRaw.List() {
		rule := cognitiveservicesaccounts.IpRule{
			Value: v.(string),
		}
		ipRules = append(ipRules, rule)
	}

	networkRules := make([]cognitiveservicesaccounts.VirtualNetworkRule, 0)
	networkRulesRaw := v["virtual_network_rules"]
	for _, v := range networkRulesRaw.(*pluginsdk.Set).List() {
		value := v.(map[string]interface{})
		subnetId := value["subnet_id"].(string)
		subnetIds = append(subnetIds, subnetId)
		rule := cognitiveservicesaccounts.VirtualNetworkRule{
			Id:                               subnetId,
			IgnoreMissingVnetServiceEndpoint: utils.Bool(value["ignore_missing_vnet_service_endpoint"].(bool)),
		}
		networkRules = append(networkRules, rule)
	}

	ruleSet := cognitiveservicesaccounts.NetworkRuleSet{
		DefaultAction:       &defaultAction,
		IpRules:             &ipRules,
		VirtualNetworkRules: &networkRules,
	}
	return &ruleSet, subnetIds
}

func expandCognitiveAccountStorage(input []interface{}) *[]cognitiveservicesaccounts.UserOwnedStorage {
	if len(input) == 0 {
		return nil
	}
	results := make([]cognitiveservicesaccounts.UserOwnedStorage, 0)
	for _, v := range input {
		value := v.(map[string]interface{})
		results = append(results, cognitiveservicesaccounts.UserOwnedStorage{
			ResourceId:       utils.String(value["storage_account_id"].(string)),
			IdentityClientId: utils.String(value["identity_client_id"].(string)),
		})
	}
	return &results
}

func expandCognitiveAccountIdentity(input []interface{}) (*legacyIdentity.SystemUserAssignedIdentityMap, error) {
	expanded, err := identity.ExpandSystemAndUserAssignedMap(input)
	if err != nil {
		return nil, err
	}

	intermediate := legacyIdentity.ExpandedConfig{
		Type: legacyIdentity.Type(string(expanded.Type)),
	}

	if expanded.Type == identity.TypeUserAssigned || expanded.Type == identity.TypeSystemAssignedUserAssigned {
		intermediate.UserAssignedIdentityIds = make([]string, 0)
		for k := range expanded.IdentityIds {
			intermediate.UserAssignedIdentityIds = append(intermediate.UserAssignedIdentityIds, k)
		}
	}

	out := legacyIdentity.SystemUserAssignedIdentityMap{}
	out.FromExpandedConfig(intermediate)
	return &out, nil
}

func expandCognitiveAccountAPIProperties(d *pluginsdk.ResourceData) (*cognitiveservicesaccounts.ApiProperties, error) {
	props := cognitiveservicesaccounts.ApiProperties{}
	kind := d.Get("kind")
	if kind == "QnAMaker" {
		if v, ok := d.GetOk("qna_runtime_endpoint"); ok && v != "" {
			props.QnaRuntimeEndpoint = utils.String(v.(string))
		} else {
			return nil, fmt.Errorf("the QnAMaker runtime endpoint `qna_runtime_endpoint` is required when kind is set to `QnAMaker`")
		}
	}
	if v, ok := d.GetOk("custom_question_answering_search_service_id"); ok {
		if kind == "TextAnalytics" {
			props.QnaAzureSearchEndpointId = utils.String(v.(string))
		} else {
			return nil, fmt.Errorf("the Search Service ID `custom_question_answering_search_service_id` can only be set when kind is set to `TextAnalytics`")
		}
	}
	if v, ok := d.GetOk("metrics_advisor_aad_client_id"); ok {
		if kind == "MetricsAdvisor" {
			props.AadClientId = utils.String(v.(string))
		} else {
			return nil, fmt.Errorf("metrics_advisor_aad_client_id can only used set when kind is set to `MetricsAdvisor`")
		}
	}
	if v, ok := d.GetOk("metrics_advisor_aad_tenant_id"); ok {
		if kind == "MetricsAdvisor" {
			props.AadTenantId = utils.String(v.(string))
		} else {
			return nil, fmt.Errorf("metrics_advisor_aad_tenant_id can only used set when kind is set to `MetricsAdvisor`")
		}
	}
	if v, ok := d.GetOk("metrics_advisor_super_user_name"); ok {
		if kind == "MetricsAdvisor" {
			props.SuperUser = utils.String(v.(string))
		} else {
			return nil, fmt.Errorf("metrics_advisor_super_user_name can only used set when kind is set to `MetricsAdvisor`")
		}
	}
	if v, ok := d.GetOk("metrics_advisor_website_name"); ok {
		if kind == "MetricsAdvisor" {
			props.WebsiteName = utils.String(v.(string))
		} else {
			return nil, fmt.Errorf("metrics_advisor_website_name can only used set when kind is set to `MetricsAdvisor`")
		}
	}
	return &props, nil
}

func flattenCognitiveAccountNetworkAcls(input *cognitiveservicesaccounts.NetworkRuleSet) []interface{} {
	if input == nil {
		return []interface{}{}
	}

	ipRules := make([]interface{}, 0)
	if input.IpRules != nil {
		for _, v := range *input.IpRules {
			ipRules = append(ipRules, v.Value)
		}
	}

	virtualNetworkSubnetIds := make([]interface{}, 0)
	virtualNetworkRules := make([]interface{}, 0)
	if input.VirtualNetworkRules != nil {
		for _, v := range *input.VirtualNetworkRules {
			id := v.Id
			subnetId, err := networkParse.SubnetIDInsensitively(v.Id)
			if err == nil {
				id = subnetId.ID()
			}

			virtualNetworkSubnetIds = append(virtualNetworkSubnetIds, id)
			virtualNetworkRules = append(virtualNetworkRules, map[string]interface{}{
				"subnet_id":                            id,
				"ignore_missing_vnet_service_endpoint": *v.IgnoreMissingVnetServiceEndpoint,
			})
		}
	}

	return []interface{}{map[string]interface{}{
		"default_action":        input.DefaultAction,
		"ip_rules":              pluginsdk.NewSet(pluginsdk.HashString, ipRules),
		"virtual_network_rules": virtualNetworkRules,
	}}
}

func flattenCognitiveAccountStorage(input *[]cognitiveservicesaccounts.UserOwnedStorage) []interface{} {
	if input == nil {
		return []interface{}{}
	}
	results := make([]interface{}, 0)
	for _, v := range *input {
		value := make(map[string]interface{})
		if v.ResourceId != nil {
			value["storage_account_id"] = *v.ResourceId
		}
		if v.IdentityClientId != nil {
			value["identity_client_id"] = *v.IdentityClientId
		}
		results = append(results, value)
	}
	return results
}

func flattenCognitiveAccountIdentity(input *legacyIdentity.SystemUserAssignedIdentityMap) (*[]interface{}, error) {
	var transform *identity.SystemAndUserAssignedMap

	if input != nil {
		expanded := input.ToExpandedConfig()
		transform = &identity.SystemAndUserAssignedMap{
			Type:        identity.Type(string(expanded.Type)),
			IdentityIds: make(map[string]identity.UserAssignedIdentityDetails),
			TenantId:    expanded.TenantId,
			PrincipalId: expanded.PrincipalId,
		}
		for _, k := range expanded.UserAssignedIdentityIds {
			transform.IdentityIds[k] = identity.UserAssignedIdentityDetails{
				// TODO: populate me once the SDK is updated
			}
		}
	}

	return identity.FlattenSystemAndUserAssignedMap(transform)
}

func resourceCognitiveAccountSchema() map[string]*pluginsdk.Schema {
	schema := map[string]*pluginsdk.Schema{
		"name": {
			Type:         pluginsdk.TypeString,
			Required:     true,
			ForceNew:     true,
			ValidateFunc: validate.CognitiveServicesAccountName(),
		},

		"location": azure.SchemaLocation(),

		"resource_group_name": azure.SchemaResourceGroupName(),

		"kind": {
			Type:     pluginsdk.TypeString,
			Required: true,
			ForceNew: true,
			ValidateFunc: validation.StringInSlice([]string{
				"Academic",
				"AnomalyDetector",
				"Bing.Autosuggest",
				"Bing.Autosuggest.v7",
				"Bing.CustomSearch",
				"Bing.Search",
				"Bing.Search.v7",
				"Bing.Speech",
				"Bing.SpellCheck",
				"Bing.SpellCheck.v7",
				"CognitiveServices",
				"ComputerVision",
				"ContentModerator",
				"CustomSpeech",
				"CustomVision.Prediction",
				"CustomVision.Training",
				"Emotion",
				"Face",
				"FormRecognizer",
				"ImmersiveReader",
				"LUIS",
				"LUIS.Authoring",
				"MetricsAdvisor",
				"Personalizer",
				"QnAMaker",
				"Recommendations",
				"SpeakerRecognition",
				"Speech",
				"SpeechServices",
				"SpeechTranslation",
				"TextAnalytics",
				"TextTranslation",
				"WebLM",
			}, false),
		},

		"sku_name": {
			Type:     pluginsdk.TypeString,
			Required: true,
			ValidateFunc: validation.StringInSlice([]string{
				"F0", "F1", "S0", "S", "S1", "S2", "S3", "S4", "S5", "S6", "P0", "P1", "P2", "E0",
			}, false),
		},

		"custom_subdomain_name": {
			Type:         pluginsdk.TypeString,
			Optional:     true,
			ForceNew:     true,
			ValidateFunc: validation.StringIsNotEmpty,
		},

		"fqdns": {
			Type:     pluginsdk.TypeList,
			Optional: true,
			Elem: &pluginsdk.Schema{
				Type:         pluginsdk.TypeString,
				ValidateFunc: validation.StringIsNotEmpty,
			},
		},

		"identity": commonschema.SystemAssignedUserAssignedIdentityOptional(),

		"local_auth_enabled": {
			Type:     pluginsdk.TypeBool,
			Optional: true,
			Default:  true,
		},

		"metrics_advisor_aad_client_id": {
			Type:         pluginsdk.TypeString,
			Optional:     true,
			ForceNew:     true,
			ValidateFunc: validation.IsUUID,
		},

		"metrics_advisor_aad_tenant_id": {
			Type:         pluginsdk.TypeString,
			Optional:     true,
			ForceNew:     true,
			ValidateFunc: validation.IsUUID,
		},

		"metrics_advisor_super_user_name": {
			Type:         pluginsdk.TypeString,
			Optional:     true,
			ForceNew:     true,
			ValidateFunc: validation.StringIsNotEmpty,
		},

		"metrics_advisor_website_name": {
			Type:         pluginsdk.TypeString,
			Optional:     true,
			ForceNew:     true,
			ValidateFunc: validation.StringIsNotEmpty,
		},

		"network_acls": {
			Type:         pluginsdk.TypeList,
			Optional:     true,
			MaxItems:     1,
			RequiredWith: []string{"custom_subdomain_name"},
			Elem: &pluginsdk.Resource{
				Schema: map[string]*pluginsdk.Schema{
					"default_action": {
						Type:     pluginsdk.TypeString,
						Required: true,
						ValidateFunc: validation.StringInSlice([]string{
							string(cognitiveservicesaccounts.NetworkRuleActionAllow),
							string(cognitiveservicesaccounts.NetworkRuleActionDeny),
						}, false),
					},
					"ip_rules": {
						Type:     pluginsdk.TypeSet,
						Optional: true,
						Elem: &pluginsdk.Schema{
							Type: pluginsdk.TypeString,
							ValidateFunc: validation.Any(
								commonValidate.IPv4Address,
								commonValidate.CIDR,
							),
						},
						Set: set.HashIPv4AddressOrCIDR,
					},

					"virtual_network_rules": {
						Type:       pluginsdk.TypeSet,
						Optional:   true,
						ConfigMode: pluginsdk.SchemaConfigModeAuto,
						Elem: &pluginsdk.Resource{
							Schema: map[string]*pluginsdk.Schema{
								"subnet_id": {
									Type:     pluginsdk.TypeString,
									Required: true,
								},

								"ignore_missing_vnet_service_endpoint": {
									Type:     pluginsdk.TypeBool,
									Optional: true,
									Default:  false,
								},
							},
						},
					},
				},
			},
		},
		"outbound_network_access_restricted": {
			Type:     pluginsdk.TypeBool,
			Optional: true,
			Default:  false,
		},

		"public_network_access_enabled": {
			Type:     pluginsdk.TypeBool,
			Optional: true,
			Default:  true,
		},

		"qna_runtime_endpoint": {
			Type:         pluginsdk.TypeString,
			Optional:     true,
			ValidateFunc: validation.IsURLWithHTTPorHTTPS,
		},

		"custom_question_answering_search_service_id": {
			Type:         pluginsdk.TypeString,
			Optional:     true,
			ValidateFunc: searchValidate.SearchServiceID,
		},

		"storage": {
			Type:     pluginsdk.TypeList,
			Optional: true,
			Elem: &pluginsdk.Resource{
				Schema: map[string]*pluginsdk.Schema{
					"storage_account_id": {
						Type:         pluginsdk.TypeString,
						Required:     true,
						ValidateFunc: storageValidate.StorageAccountID,
					},

					"identity_client_id": {
						Type:         pluginsdk.TypeString,
						Optional:     true,
						ValidateFunc: validation.IsUUID,
					},
				},
			},
		},

		"tags": tags.Schema(),

		"endpoint": {
			Type:     pluginsdk.TypeString,
			Computed: true,
		},

		"primary_access_key": {
			Type:      pluginsdk.TypeString,
			Computed:  true,
			Sensitive: true,
		},

		"secondary_access_key": {
			Type:      pluginsdk.TypeString,
			Computed:  true,
			Sensitive: true,
		},
	}
	return schema
}
