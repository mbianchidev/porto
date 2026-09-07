package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	managedKubeconfigExtension = "porto.dev/managed"
	kubeconfigLockTimeout      = 30 * time.Second
)

var (
	errEmptyKubeconfig            = errors.New("kubeconfig is empty")
	errKubeconfigContextCollision = errors.New("Kubernetes context registration collision")
)

type KubeconfigRegistration struct {
	Context string
	Path    string
	Backup  string
}

type KubeconfigOwner struct {
	Cluster  string
	Provider string
}

type KubeconfigRegistry struct {
	path string
}

func NewKubeconfigRegistry(path string) *KubeconfigRegistry {
	return &KubeconfigRegistry{path: path}
}

func KubeconfigContextName(path string) (string, error) {
	document, err := readKubeconfigDocument(path)
	if err != nil {
		return "", err
	}
	contextName, ok := document["current-context"].(string)
	if !ok || contextName == "" {
		return "", errors.New("current-context is missing")
	}
	return contextName, nil
}

func (r *KubeconfigRegistry) CheckAvailable(
	ctx context.Context,
	contextName string,
	owners ...KubeconfigOwner,
) error {
	owner := firstKubeconfigOwner(owners)
	return r.withLock(ctx, func(targetPath string) error {
		target, err := readOptionalKubeconfigDocument(targetPath)
		if err != nil {
			return fmt.Errorf("read global kubeconfig: %w", err)
		}
		if target == nil {
			return nil
		}
		if contextEntry, found := findKubeconfigNamedEntry(target, "contexts", contextName); found {
			if isManagedKubeconfigContext(contextEntry) {
				existingOwner := managedKubeconfigOwner(contextEntry)
				if kubeconfigOwnersCompatible(existingOwner, owner) {
					return nil
				}
				return kubeconfigOwnerCollision(contextName, existingOwner, owner)
			}
			return fmt.Errorf(
				"%w: global kubeconfig context %q already exists and is not managed by Porto",
				errKubeconfigContextCollision,
				contextName,
			)
		}
		for _, field := range []string{"clusters", "users"} {
			if _, found := findKubeconfigNamedEntry(target, field, contextName); found {
				return fmt.Errorf(
					"%w: global kubeconfig %s entry %q already exists and is not managed by Porto",
					errKubeconfigContextCollision,
					field,
					contextName,
				)
			}
		}
		return nil
	})
}

func (r *KubeconfigRegistry) Register(
	ctx context.Context,
	sourcePath string,
	owners ...KubeconfigOwner,
) (KubeconfigRegistration, error) {
	owner := firstKubeconfigOwner(owners)
	var registration KubeconfigRegistration
	err := r.withLock(ctx, func(targetPath string) error {
		var operationErr error
		registration, operationErr = r.registerUnlocked(targetPath, sourcePath, owner)
		return operationErr
	})
	return registration, err
}

func (r *KubeconfigRegistry) registerUnlocked(
	targetPath string,
	sourcePath string,
	owner KubeconfigOwner,
) (KubeconfigRegistration, error) {
	source, err := readKubeconfigDocument(sourcePath)
	if err != nil {
		return KubeconfigRegistration{}, fmt.Errorf("read Porto kubeconfig: %w", err)
	}
	bundle, err := kubeconfigBundleFromDocument(source)
	if err != nil {
		return KubeconfigRegistration{}, fmt.Errorf("read Porto kubeconfig entries: %w", err)
	}
	target, err := readOptionalKubeconfigDocument(targetPath)
	if err != nil {
		return KubeconfigRegistration{}, fmt.Errorf("read global kubeconfig: %w", err)
	}
	if target == nil {
		target = newKubeconfigDocument()
	}
	if err := mergeKubeconfigBundle(target, bundle, owner); err != nil {
		return KubeconfigRegistration{}, err
	}
	backup, err := writeKubeconfigDocument(targetPath, r.path+".porto-backup", target)
	if err != nil {
		return KubeconfigRegistration{}, err
	}
	return KubeconfigRegistration{Context: bundle.contextName, Path: r.path, Backup: backup}, nil
}

func (r *KubeconfigRegistry) Rename(
	ctx context.Context,
	oldContext string,
	sourcePath string,
	owners ...KubeconfigOwner,
) (KubeconfigRegistration, error) {
	oldOwner, newOwner := renameKubeconfigOwners(owners)
	var registration KubeconfigRegistration
	err := r.withLock(ctx, func(targetPath string) error {
		var operationErr error
		registration, operationErr = r.renameUnlocked(
			targetPath,
			oldContext,
			sourcePath,
			oldOwner,
			newOwner,
		)
		return operationErr
	})
	return registration, err
}

func (r *KubeconfigRegistry) renameUnlocked(
	targetPath string,
	oldContext string,
	sourcePath string,
	oldOwner KubeconfigOwner,
	newOwner KubeconfigOwner,
) (KubeconfigRegistration, error) {
	source, err := readKubeconfigDocument(sourcePath)
	if err != nil {
		return KubeconfigRegistration{}, fmt.Errorf("read Porto kubeconfig: %w", err)
	}
	bundle, err := kubeconfigBundleFromDocument(source)
	if err != nil {
		return KubeconfigRegistration{}, fmt.Errorf("read Porto kubeconfig entries: %w", err)
	}
	target, err := readOptionalKubeconfigDocument(targetPath)
	if err != nil {
		return KubeconfigRegistration{}, fmt.Errorf("read global kubeconfig: %w", err)
	}
	if target == nil {
		target = newKubeconfigDocument()
	}
	if oldContext != bundle.contextName {
		if oldEntry, found := findKubeconfigNamedEntry(target, "contexts", oldContext); found {
			if !isManagedKubeconfigContext(oldEntry) {
				return KubeconfigRegistration{}, fmt.Errorf(
					"%w: global kubeconfig context %q already exists and is not managed by Porto",
					errKubeconfigContextCollision,
					oldContext,
				)
			}
			existingOwner := managedKubeconfigOwner(oldEntry)
			if !kubeconfigOwnersCompatible(existingOwner, oldOwner) {
				return KubeconfigRegistration{}, kubeconfigOwnerCollision(
					oldContext,
					existingOwner,
					oldOwner,
				)
			}
			oldBundle, bundleErr := kubeconfigBundleForContext(target, oldContext)
			if bundleErr != nil {
				return KubeconfigRegistration{}, fmt.Errorf("read old managed kubeconfig entries: %w", bundleErr)
			}
			removeKubeconfigBundle(target, oldBundle)
		}
	}
	if err := mergeKubeconfigBundle(target, bundle, newOwner); err != nil {
		return KubeconfigRegistration{}, err
	}
	if target["current-context"] == oldContext {
		target["current-context"] = bundle.contextName
	}
	backup, err := writeKubeconfigDocument(targetPath, r.path+".porto-backup", target)
	if err != nil {
		return KubeconfigRegistration{}, err
	}
	return KubeconfigRegistration{Context: bundle.contextName, Path: r.path, Backup: backup}, nil
}

func mergeKubeconfigBundle(
	target map[string]any,
	bundle kubeconfigBundle,
	owner KubeconfigOwner,
) error {
	existingContext, contextFound := findKubeconfigNamedEntry(target, "contexts", bundle.contextName)
	if contextFound && isManagedKubeconfigContext(existingContext) {
		existingOwner := managedKubeconfigOwner(existingContext)
		if !kubeconfigOwnersCompatible(existingOwner, owner) {
			return kubeconfigOwnerCollision(bundle.contextName, existingOwner, owner)
		}
		if kubeconfigOwnerEmpty(owner) {
			owner = existingOwner
		}
	}
	replaceExisting := contextFound &&
		(isManagedKubeconfigContext(existingContext) || kubeconfigBundleMatches(target, bundle))
	if contextFound && !replaceExisting {
		return fmt.Errorf(
			"%w: global kubeconfig context %q already exists and is not managed by Porto",
			errKubeconfigContextCollision,
			bundle.contextName,
		)
	}
	if err := validateKubeconfigEntryReplacement(
		target,
		"clusters",
		bundle.clusterName,
		bundle.cluster,
		replaceExisting,
	); err != nil {
		return err
	}
	if err := validateKubeconfigEntryReplacement(
		target,
		"users",
		bundle.userName,
		bundle.user,
		replaceExisting,
	); err != nil {
		return err
	}
	markManagedKubeconfigContext(bundle.context, owner)
	upsertKubeconfigEntry(target, "clusters", bundle.clusterName, bundle.cluster)
	upsertKubeconfigEntry(target, "users", bundle.userName, bundle.user)
	upsertKubeconfigEntry(target, "contexts", bundle.contextName, bundle.context)
	if currentContext, _ := target["current-context"].(string); currentContext == "" {
		target["current-context"] = bundle.contextName
	}
	return nil
}

func (r *KubeconfigRegistry) Remove(
	ctx context.Context,
	contextName string,
	sourcePath string,
	owners ...KubeconfigOwner,
) error {
	owner := firstKubeconfigOwner(owners)
	return r.withLock(ctx, func(targetPath string) error {
		return r.removeUnlocked(targetPath, contextName, sourcePath, owner)
	})
}

func (r *KubeconfigRegistry) Prune(ctx context.Context, desiredContexts map[string]struct{}) error {
	return r.withLock(ctx, func(targetPath string) error {
		target, err := readOptionalKubeconfigDocument(targetPath)
		if err != nil {
			return fmt.Errorf("read global kubeconfig: %w", err)
		}
		if target == nil {
			return nil
		}
		contexts, _ := target["contexts"].([]any)
		var stale []string
		for _, rawContext := range contexts {
			contextEntry, ok := rawContext.(map[string]any)
			if !ok || !isManagedKubeconfigContext(contextEntry) {
				continue
			}
			contextName, _ := contextEntry["name"].(string)
			if _, keep := desiredContexts[contextName]; !keep {
				stale = append(stale, contextName)
			}
		}
		if len(stale) == 0 {
			return nil
		}
		for _, contextName := range stale {
			bundle, bundleErr := kubeconfigBundleForContext(target, contextName)
			if bundleErr != nil {
				return fmt.Errorf("read stale managed kubeconfig entries: %w", bundleErr)
			}
			removeKubeconfigBundle(target, bundle)
			if target["current-context"] == contextName {
				target["current-context"] = ""
			}
		}
		_, err = writeKubeconfigDocument(targetPath, r.path+".porto-backup", target)
		return err
	})
}

func (r *KubeconfigRegistry) removeUnlocked(
	targetPath string,
	contextName string,
	sourcePath string,
	owner KubeconfigOwner,
) error {
	target, err := readOptionalKubeconfigDocument(targetPath)
	if err != nil {
		return fmt.Errorf("read global kubeconfig: %w", err)
	}
	if target == nil {
		return nil
	}
	contextEntry, found := findKubeconfigNamedEntry(target, "contexts", contextName)
	if !found {
		return nil
	}
	managed := isManagedKubeconfigContext(contextEntry)
	if !managed && sourcePath != "" {
		source, sourceErr := readKubeconfigDocument(sourcePath)
		if sourceErr != nil {
			return fmt.Errorf("read Porto kubeconfig: %w", sourceErr)
		}
		sourceBundle, sourceErr := kubeconfigBundleFromDocument(source)
		if sourceErr != nil {
			return fmt.Errorf("read Porto kubeconfig entries: %w", sourceErr)
		}
		managed = sourceBundle.contextName == contextName && kubeconfigBundleMatches(target, sourceBundle)
	}
	if !managed {
		return nil
	}
	existingOwner := managedKubeconfigOwner(contextEntry)
	if !kubeconfigOwnersCompatible(existingOwner, owner) {
		return kubeconfigOwnerCollision(contextName, existingOwner, owner)
	}
	bundle, err := kubeconfigBundleForContext(target, contextName)
	if err != nil {
		return fmt.Errorf("read managed global kubeconfig entries: %w", err)
	}
	removeKubeconfigBundle(target, bundle)
	if target["current-context"] == contextName {
		target["current-context"] = ""
	}
	_, err = writeKubeconfigDocument(targetPath, r.path+".porto-backup", target)
	return err
}

func (r *KubeconfigRegistry) withLock(ctx context.Context, operation func(targetPath string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.path == "" {
		return errors.New("global kubeconfig path is empty")
	}
	targetPath, err := resolveKubeconfigWritePath(r.path)
	if err != nil {
		return fmt.Errorf("resolve global kubeconfig path: %w", err)
	}
	lockContext, cancel := context.WithTimeout(ctx, kubeconfigLockTimeout)
	defer cancel()
	lock, err := acquireKubeconfigFileLock(lockContext, r.path+".lock")
	if err != nil {
		return fmt.Errorf("lock global kubeconfig: %w", err)
	}
	operationErr := operation(targetPath)
	closeErr := lock.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("unlock global kubeconfig: %w", closeErr)
	}
	return errors.Join(operationErr, closeErr)
}

func removeKubeconfigBundle(document map[string]any, bundle kubeconfigBundle) {
	removeKubeconfigNamedEntry(document, "contexts", bundle.contextName)
	if !kubeconfigEntryReferencedByOtherContext(document, "cluster", bundle.clusterName, "") {
		removeKubeconfigNamedEntry(document, "clusters", bundle.clusterName)
	}
	if !kubeconfigEntryReferencedByOtherContext(document, "user", bundle.userName, "") {
		removeKubeconfigNamedEntry(document, "users", bundle.userName)
	}
}

func writeKubeconfigDocument(path, backupPath string, document map[string]any) (string, error) {
	contents, err := yaml.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode global kubeconfig: %w", err)
	}
	backup := ""
	current, readErr := os.ReadFile(path)
	if readErr == nil {
		backup = backupPath
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			if err := writeFileAtomic(backup, current); err != nil {
				return "", fmt.Errorf("backup global kubeconfig: %w", err)
			}
		} else if err != nil {
			return "", fmt.Errorf("inspect global kubeconfig backup: %w", err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", fmt.Errorf("read global kubeconfig backup source: %w", readErr)
	}
	if err := writeFileAtomic(path, contents); err != nil {
		return "", fmt.Errorf("write global kubeconfig: %w", err)
	}
	return backup, nil
}

func resolveKubeconfigWritePath(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

type kubeconfigBundle struct {
	contextName string
	clusterName string
	userName    string
	context     map[string]any
	cluster     map[string]any
	user        map[string]any
}

func readKubeconfigDocument(path string) (map[string]any, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, errEmptyKubeconfig
	}
	return document, nil
}

func readOptionalKubeconfigDocument(path string) (map[string]any, error) {
	document, err := readKubeconfigDocument(path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errEmptyKubeconfig) {
		return nil, nil
	}
	return document, err
}

func newKubeconfigDocument() map[string]any {
	return map[string]any{
		"apiVersion":      "v1",
		"kind":            "Config",
		"current-context": "",
		"clusters":        []any{},
		"contexts":        []any{},
		"users":           []any{},
	}
}

func kubeconfigBundleFromDocument(document map[string]any) (kubeconfigBundle, error) {
	contextName, ok := document["current-context"].(string)
	if !ok || contextName == "" {
		return kubeconfigBundle{}, errors.New("current-context is missing")
	}
	return kubeconfigBundleForContext(document, contextName)
}

func kubeconfigBundleForContext(document map[string]any, contextName string) (kubeconfigBundle, error) {
	contextEntry, err := kubeconfigNamedEntry(document, "contexts", contextName)
	if err != nil {
		return kubeconfigBundle{}, err
	}
	contextValue, ok := contextEntry["context"].(map[string]any)
	if !ok {
		return kubeconfigBundle{}, fmt.Errorf("context %q has no configuration", contextName)
	}
	clusterName, ok := contextValue["cluster"].(string)
	if !ok || clusterName == "" {
		return kubeconfigBundle{}, fmt.Errorf("context %q has no cluster", contextName)
	}
	userName, ok := contextValue["user"].(string)
	if !ok || userName == "" {
		return kubeconfigBundle{}, fmt.Errorf("context %q has no user", contextName)
	}
	clusterEntry, err := kubeconfigNamedEntry(document, "clusters", clusterName)
	if err != nil {
		return kubeconfigBundle{}, err
	}
	userEntry, err := kubeconfigNamedEntry(document, "users", userName)
	if err != nil {
		return kubeconfigBundle{}, err
	}
	return kubeconfigBundle{
		contextName: contextName,
		clusterName: clusterName,
		userName:    userName,
		context:     contextEntry,
		cluster:     clusterEntry,
		user:        userEntry,
	}, nil
}

func rewriteKubeconfigServer(data []byte, server string) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode Kubernetes kubeconfig: %w", err)
	}
	contextName, ok := document["current-context"].(string)
	if !ok || contextName == "" {
		return nil, errors.New("current-context is missing")
	}
	contextEntry, err := kubeconfigNamedEntry(document, "contexts", contextName)
	if err != nil {
		return nil, err
	}
	contextValue, ok := contextEntry["context"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("context %q has no configuration", contextName)
	}
	clusterName, ok := contextValue["cluster"].(string)
	if !ok || clusterName == "" {
		return nil, fmt.Errorf("context %q has no cluster", contextName)
	}
	clusterEntry, err := kubeconfigNamedEntry(document, "clusters", clusterName)
	if err != nil {
		return nil, err
	}
	clusterValue, ok := clusterEntry["cluster"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cluster %q has no configuration", clusterName)
	}
	clusterValue["server"] = server
	contents, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode Kubernetes kubeconfig: %w", err)
	}
	return contents, nil
}

func kubeconfigBundleMatches(document map[string]any, expected kubeconfigBundle) bool {
	actual, err := kubeconfigBundleForContext(document, expected.contextName)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(actual.context, expected.context) &&
		reflect.DeepEqual(actual.cluster, expected.cluster) &&
		reflect.DeepEqual(actual.user, expected.user)
}

func validateKubeconfigEntryReplacement(
	document map[string]any,
	field string,
	name string,
	replacement map[string]any,
	replaceExisting bool,
) error {
	existing, found := findKubeconfigNamedEntry(document, field, name)
	if !found || reflect.DeepEqual(existing, replacement) {
		return nil
	}
	if replaceExisting {
		return nil
	}
	return fmt.Errorf(
		"%w: global kubeconfig %s entry %q already exists and is not exclusively managed by Porto",
		errKubeconfigContextCollision,
		field,
		name,
	)
}

func kubeconfigEntryReferencedByOtherContext(
	document map[string]any,
	contextField string,
	name string,
	excludedContext string,
) bool {
	contexts, _ := document["contexts"].([]any)
	for _, rawContext := range contexts {
		contextEntry, ok := rawContext.(map[string]any)
		if !ok || contextEntry["name"] == excludedContext {
			continue
		}
		contextValue, ok := contextEntry["context"].(map[string]any)
		if ok && contextValue[contextField] == name {
			return true
		}
	}
	return false
}

func kubeconfigNamedEntry(document map[string]any, field, name string) (map[string]any, error) {
	entry, found := findKubeconfigNamedEntry(document, field, name)
	if found {
		return entry, nil
	}
	if _, ok := document[field].([]any); !ok {
		return nil, fmt.Errorf("%s is missing", field)
	}
	return nil, fmt.Errorf("%s entry %q is missing", field, name)
}

func findKubeconfigNamedEntry(document map[string]any, field, name string) (map[string]any, bool) {
	entries, ok := document[field].([]any)
	if !ok {
		return nil, false
	}
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if ok && entry["name"] == name {
			return entry, true
		}
	}
	return nil, false
}

func upsertKubeconfigEntry(document map[string]any, field, name string, replacement map[string]any) {
	entries, _ := document[field].([]any)
	for index, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if ok && entry["name"] == name {
			entries[index] = replacement
			document[field] = entries
			return
		}
	}
	document[field] = append(entries, replacement)
}

func removeKubeconfigNamedEntry(document map[string]any, field, name string) {
	entries, _ := document[field].([]any)
	filtered := entries[:0]
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if ok && entry["name"] == name {
			continue
		}
		filtered = append(filtered, rawEntry)
	}
	document[field] = filtered
}

func markManagedKubeconfigContext(contextEntry map[string]any, owner KubeconfigOwner) {
	contextValue, ok := contextEntry["context"].(map[string]any)
	if !ok {
		return
	}
	extensions, _ := contextValue["extensions"].([]any)
	for _, rawExtension := range extensions {
		extension, ok := rawExtension.(map[string]any)
		if ok && extension["name"] == managedKubeconfigExtension {
			updateManagedKubeconfigExtension(extension, owner)
			return
		}
	}
	extension := map[string]any{
		"name": managedKubeconfigExtension,
		"extension": map[string]any{
			"managed": true,
		},
	}
	updateManagedKubeconfigExtension(extension, owner)
	contextValue["extensions"] = append(extensions, extension)
}

func isManagedKubeconfigContext(contextEntry map[string]any) bool {
	contextValue, ok := contextEntry["context"].(map[string]any)
	if !ok {
		return false
	}
	extensions, _ := contextValue["extensions"].([]any)
	for _, rawExtension := range extensions {
		extension, ok := rawExtension.(map[string]any)
		if ok && extension["name"] == managedKubeconfigExtension {
			return true
		}
	}
	return false
}

func updateManagedKubeconfigExtension(extension map[string]any, owner KubeconfigOwner) {
	value, ok := extension["extension"].(map[string]any)
	if !ok {
		value = map[string]any{}
		extension["extension"] = value
	}
	value["managed"] = true
	if owner.Cluster != "" {
		value["cluster"] = owner.Cluster
	}
	if owner.Provider != "" {
		value["provider"] = owner.Provider
	}
}

func managedKubeconfigOwner(contextEntry map[string]any) KubeconfigOwner {
	contextValue, ok := contextEntry["context"].(map[string]any)
	if !ok {
		return KubeconfigOwner{}
	}
	extensions, _ := contextValue["extensions"].([]any)
	for _, rawExtension := range extensions {
		extension, ok := rawExtension.(map[string]any)
		if !ok || extension["name"] != managedKubeconfigExtension {
			continue
		}
		value, _ := extension["extension"].(map[string]any)
		cluster, _ := value["cluster"].(string)
		provider, _ := value["provider"].(string)
		return KubeconfigOwner{Cluster: cluster, Provider: provider}
	}
	return KubeconfigOwner{}
}

func firstKubeconfigOwner(owners []KubeconfigOwner) KubeconfigOwner {
	if len(owners) == 0 {
		return KubeconfigOwner{}
	}
	return owners[0]
}

func renameKubeconfigOwners(owners []KubeconfigOwner) (KubeconfigOwner, KubeconfigOwner) {
	switch len(owners) {
	case 0:
		return KubeconfigOwner{}, KubeconfigOwner{}
	case 1:
		return KubeconfigOwner{}, owners[0]
	default:
		return owners[0], owners[1]
	}
}

func kubeconfigOwnerEmpty(owner KubeconfigOwner) bool {
	return owner.Cluster == "" && owner.Provider == ""
}

func kubeconfigOwnersCompatible(existing, requested KubeconfigOwner) bool {
	return kubeconfigOwnerEmpty(existing) ||
		kubeconfigOwnerEmpty(requested) ||
		existing == requested
}

func kubeconfigOwnerCollision(
	contextName string,
	existing KubeconfigOwner,
	requested KubeconfigOwner,
) error {
	return fmt.Errorf(
		"%w: global kubeconfig context %q is managed for %s/%s, not %s/%s",
		errKubeconfigContextCollision,
		contextName,
		existing.Provider,
		existing.Cluster,
		requested.Provider,
		requested.Cluster,
	)
}
