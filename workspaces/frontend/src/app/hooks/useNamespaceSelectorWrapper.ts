import { Namespace, useNamespaceSelector } from 'mod-arch-core';

type UseNamespaceSelectorWrapperReturn = Omit<
  ReturnType<typeof useNamespaceSelector>,
  'preferredNamespace'
> & {
  selectedNamespace: Namespace['name'];
};

export const useNamespaceSelectorWrapper = (): UseNamespaceSelectorWrapperReturn => {
  const { namespaces, preferredNamespace, ...rest } = useNamespaceSelector({
    storageKey: 'kubeflow.notebooks.namespace.lastUsed',
    storeLastNamespace: true,
  });

  const defaultNs =
    namespaces.find((n) => n.name === 'default')?.name ?? namespaces[0]?.name ?? 'default';

  return {
    namespaces,
    preferredNamespace,
    ...rest,
    selectedNamespace: preferredNamespace?.name || defaultNs,
  };
};

