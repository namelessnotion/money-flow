<script setup lang="ts">
import { useQuery } from '@vue/apollo-composable'
import { computed } from 'vue'
import { formatDateTime as formatDate } from '../lib/dates'
import { ENTITIES_QUERY, PAGE_SIZE, type EntitiesQueryResult, type EntitiesQueryVariables } from '../graphql/entities'

const { result, loading, error, fetchMore } = useQuery<EntitiesQueryResult, EntitiesQueryVariables>(
  ENTITIES_QUERY,
  { variables: { first: PAGE_SIZE } },
)

const entities = computed(() => result.value?.entities.edges.map((edge) => edge.node) ?? [])
const pageInfo = computed(() => result.value?.entities.pageInfo)
const isLoadingMore = computed(() => loading.value && entities.value.length > 0)


async function loadMore(): Promise<void> {
  const endCursor = pageInfo.value?.endCursor
  if (!endCursor) return

  await fetchMore({ variables: { first: PAGE_SIZE, after: endCursor } })
}
</script>

<template>
  <section class="w-full max-w-3xl">
    <header class="mb-6">
      <p class="text-sm font-bold uppercase tracking-wider text-slate-500">Money Flow</p>
      <h1 class="text-4xl font-bold tracking-tight text-slate-900">Entities</h1>
    </header>

    <div v-if="error" class="rounded-lg bg-rose-50 p-4 text-rose-700">
      Unable to load entities: {{ error.message }}
    </div>

    <div v-else-if="loading && entities.length === 0" class="rounded-lg bg-slate-100 p-4 text-slate-600">
      Loading entities…
    </div>

    <div v-else-if="entities.length === 0" class="rounded-lg bg-slate-100 p-4 text-slate-600">
      No entities have been onboarded yet.
    </div>

    <template v-else>
      <ul class="divide-y divide-slate-200 overflow-hidden rounded-lg border border-slate-200 bg-white shadow-sm">
        <li
          v-for="entity in entities"
          :key="entity!.id"
          class="flex items-center justify-between gap-4 px-4 py-3"
        >
          <div class="min-w-0">
            <RouterLink
              :to="{ name: 'entity', params: { entityId: entity!.id } }"
              class="block truncate font-semibold text-blue-700 hover:underline"
            >
              {{ entity!.name }}
            </RouterLink>
            <p class="truncate text-sm text-slate-500">{{ entity!.holderUuid }}</p>
          </div>
          <p class="shrink-0 text-sm text-slate-500">{{ formatDate(entity!.createdAt) }}</p>
        </li>
      </ul>

      <div class="mt-4 flex justify-center">
        <button
          v-if="pageInfo?.hasNextPage"
          type="button"
          :disabled="isLoadingMore"
          class="rounded-lg bg-blue-700 px-4 py-2 font-bold text-white hover:bg-blue-800 disabled:cursor-wait disabled:opacity-60"
          @click="loadMore"
        >
          {{ isLoadingMore ? 'Loading…' : 'Load more' }}
        </button>
        <p v-else class="text-sm text-slate-500">All entities loaded.</p>
      </div>
    </template>
  </section>
</template>
