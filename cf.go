package main

import (
	"fmt"

	"github.com/deis/steward/cf"
	"github.com/deis/steward/k8s"
	"github.com/juju/loggo"
	"k8s.io/kubernetes/pkg/client/restclient"
)

type errServiceAlreadyPublished struct {
	entry *k8s.ServiceCatalogEntry
}

func (e errServiceAlreadyPublished) Error() string {
	return fmt.Sprintf("duplicate service catalog entry: %s", e.entry.ResourceName())
}

// does the following:
//
//	1. fetches the service catalog from cloud foundry
//	2. checks the 3pr for already-existing entries, and errors if one already exists
//	3. if none error-ed in #2, publishes 3prs for all of the catalog entries
//
// returns all of the entries it wrote into the catalog, or an error
func publishCloudFoundryCatalog(logger loggo.Logger, cl *cf.Client, restCl *restclient.RESTClient) ([]*k8s.ServiceCatalogEntry, error) {
	// get catalog from cloud foundry
	cfServices, err := cf.GetCatalog(logger, cl)
	if err != nil {
		logger.Debugf("error getting CF catalog (%s)", err)
		return nil, err
	}
	// get existing catalog from 3pr
	catalogEntries, err := k8s.GetServiceCatalogEntries(logger, restCl)
	if err != nil {
		logger.Debugf("error getting existing service catalog entries from k8s (%s)", err)
		return nil, err
	}
	// create in-mem lookup table from 3pr catalog, check for duplicate entries in cf catalog
	lookup := k8s.NewServiceCatalogLookup(catalogEntries)
	for _, cfService := range cfServices {
		for _, plan := range cfService.Plans {
			entry := &k8s.ServiceCatalogEntry{Info: cfService.ServiceInfo, Plan: plan}
			if lookup.Get(entry) != nil {
				return nil, errServiceAlreadyPublished{entry: entry}
			}
			lookup.Set(entry)
		}
	}
	published := []*k8s.ServiceCatalogEntry{}
	// write all entries from cf catalog to 3pr
	for _, cfService := range cfServices {
		for _, plan := range cfService.Plans {
			entry := &k8s.ServiceCatalogEntry{Info: cfService.ServiceInfo, Plan: plan}
			if err := k8s.PublishServiceCatalogEntry(restCl, entry); err != nil {
				logger.Errorf("error publishing catalog entry %s (%s), continuing", entry.ResourceName(), err)
				continue
			}
			published = append(published, entry)
		}
	}

	return published, nil
}