package provider

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/volume"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

const (
	volumeReadRefreshTimeout             = 30 * time.Second
	volumeReadRefreshWaitBeforeRefreshes = 5 * time.Second
	volumeReadRefreshDelay               = 2 * time.Second
)

func resourceDockerVolume() *schema.Resource {
	return &schema.Resource{
		Description: "Creates and destroys a volume in Docker. This can be used alongside [docker_container](container.md) to prepare volumes that can be shared across containers.",

		CreateContext: resourceDockerVolumeCreate,
		ReadContext:   resourceDockerVolumeRead,
		DeleteContext: resourceDockerVolumeDelete,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:        schema.TypeString,
				Description: "The name of the Docker volume (will be generated if not provided).",
				Optional:    true,
				Computed:    true,
				ForceNew:    true,
			},
			"labels": {
				Type:        schema.TypeSet,
				Description: "User-defined key/value metadata",
				Optional:    true,
				ForceNew:    true,
				Elem:        labelSchema,
			},
			"driver": {
				Type:             schema.TypeString,
				Description:      "Driver type for the volume. Defaults to `local`.",
				Optional:         true,
				Computed:         true,
				ForceNew:         true,
				DiffSuppressFunc: suppressIfOnlyLatestHasBeenAddedToDriver(),
			},
			"driver_opts": {
				Type:        schema.TypeMap,
				Description: "Options specific to the driver.",
				Optional:    true,
				ForceNew:    true,
			},
			"mountpoint": {
				Type:        schema.TypeString,
				Description: "The mountpoint of the volume.",
				Computed:    true,
			},
		},
		SchemaVersion: 1,
		StateUpgraders: []schema.StateUpgrader{
			{
				Version: 0,
				Type:    resourceDockerVolumeV0().CoreConfigSchema().ImpliedType(),
				Upgrade: func(ctx context.Context, rawState map[string]interface{}, meta interface{}) (map[string]interface{}, error) {
					return replaceLabelsMapFieldWithSetField(rawState), nil
				},
			},
		},
	}
}

func resourceDockerVolumeCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*ProviderConfig).DockerClient

	createOpts := volume.VolumeCreateBody{}

	if v, ok := d.GetOk("name"); ok {
		createOpts.Name = v.(string)
	}
	if v, ok := d.GetOk("labels"); ok {
		createOpts.Labels = labelSetToMap(v.(*schema.Set))
	}
	if v, ok := d.GetOk("driver"); ok {
		createOpts.Driver = v.(string)
	}
	if v, ok := d.GetOk("driver_opts"); ok {
		createOpts.DriverOpts = mapTypeMapValsToString(v.(map[string]interface{}))
	}

	var err error
	var retVolume types.Volume
	retVolume, err = client.VolumeCreate(ctx, createOpts)

	if err != nil {
		return diag.Errorf("Unable to create volume: %s", err)
	}

	d.SetId(retVolume.Name)
	return resourceDockerVolumeRead(ctx, d, meta)
}

func resourceDockerVolumeRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	log.Printf("[INFO] Waiting for volume: '%s' to expose all fields: max '%v seconds'", d.Id(), networkReadRefreshTimeout)

	stateConf := &resource.StateChangeConf{
		Pending:    []string{"pending"},
		Target:     []string{"all_fields", "removed"},
		Refresh:    resourceDockerVolumeReadRefreshFunc(ctx, d, meta),
		Timeout:    volumeReadRefreshTimeout,
		MinTimeout: volumeReadRefreshWaitBeforeRefreshes,
		Delay:      volumeReadRefreshDelay,
	}

	// Wait, catching any errors
	_, err := stateConf.WaitForStateContext(ctx)
	if err != nil {
		return diag.FromErr(err)
	}

	return nil
}

func resourceDockerVolumeDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	log.Printf("[INFO] Waiting for volume: '%s' to get removed: max '%v seconds'", d.Id(), volumeReadRefreshTimeout)

	stateConf := &resource.StateChangeConf{
		Pending:    []string{"in_use"},
		Target:     []string{"removed"},
		Refresh:    resourceDockerVolumeRemoveRefreshFunc(d.Id(), meta),
		Timeout:    volumeReadRefreshTimeout,
		MinTimeout: volumeReadRefreshWaitBeforeRefreshes,
		Delay:      volumeReadRefreshDelay,
	}

	// Wait, catching any errors
	_, err := stateConf.WaitForStateContext(ctx)
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId("")
	return nil
}

func resourceDockerVolumeReadRefreshFunc(ctx context.Context,
	d *schema.ResourceData, meta interface{}) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {
		client := meta.(*ProviderConfig).DockerClient
		volumeID := d.Id()

		retVolume, _, err := client.VolumeInspectWithRaw(ctx, volumeID)
		if err != nil {
			log.Printf("[WARN] Volume (%s) not found, removing from state", volumeID)
			d.SetId("")
			return volumeID, "removed", nil
		}

		jsonObj, _ := json.MarshalIndent(retVolume, "", "\t")
		log.Printf("[DEBUG] Docker volume inspect from readFunc: %s", jsonObj)

		d.Set("name", retVolume.Name)
		d.Set("labels", mapToLabelSet(retVolume.Labels))
		d.Set("driver", retVolume.Driver)
		d.Set("driver_opts", retVolume.Options)
		d.Set("mountpoint", retVolume.Mountpoint)

		log.Println("[DEBUG] all volume fields exposed")
		return volumeID, "all_fields", nil
	}
}

func resourceDockerVolumeRemoveRefreshFunc(
	volumeID string, meta interface{}) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {
		client := meta.(*ProviderConfig).DockerClient
		forceDelete := true

		if err := client.VolumeRemove(context.Background(), volumeID, forceDelete); err != nil {
			if containsIgnorableErrorMessage(err.Error(), "volume is in use") {
				log.Printf("[INFO] Volume with id '%v' is still in use", volumeID)
				return volumeID, "in_use", nil
			}
			log.Printf("[INFO] Removing volume with id '%v' caused an error: %v", volumeID, err)
			return nil, "", err
		}
		log.Printf("[INFO] Removing volume with id '%v' got removed", volumeID)
		return volumeID, "removed", nil
	}
}

func suppressIfOnlyLatestHasBeenAddedToDriver() schema.SchemaDiffSuppressFunc {
	return func(k, old, new string, d *schema.ResourceData) bool {
		oldParts := strings.Split(old, ":")
		newParts := strings.Split(new, ":")
		if len(oldParts) == len(newParts)+1 {
			log.Printf("Cluster seems to have added a tag when we didn't provide one.")
			addedTag := oldParts[len(oldParts)-1]
			if addedTag == "latest" {
				log.Printf("Cluster added the latest tag. Ignoring this change, if nothing else changed.")
				return strings.Join(oldParts[:len(oldParts)-1], ":") == strings.Join(newParts, ":")
			}
			log.Printf("The added tag was not 'latest' but instead '%s'. This looks like an actual change, so not ignoring it.", addedTag)
		}

		return old == new
	}
}
