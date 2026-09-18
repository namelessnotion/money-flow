<script setup lang="ts">
import { computed } from 'vue'
import { useQuery } from '@vue/apollo-composable'
import AccountList from '../components/AccountList.vue'
import AchTransactionList from '../components/AchTransactionList.vue'
import InitiateAchForm from '../components/InitiateAchForm.vue'
import { ENTITY_QUERY, type EntityQueryResult, type EntityQueryVariables } from '../graphql/entity'

const props = defineProps<{ entityId: string }>()

const { result, loading, error } = useQuery<EntityQueryResult, EntityQueryVariables>(ENTITY_QUERY, {
  variables: () => ({ id: props.entityId }),
})

const entity = computed(() => result.value?.entity)
</script>

<template>
  <RouterLink :to="{ name: 'entities' }" class="text-sm font-bold text-blue-700 hover:underline">
    ← All entities
  </RouterLink>

  <div v-if="error" class="mt-6 rounded-lg bg-rose-50 p-4 text-rose-700">Unable to load entity: {{ error.message }}</div>
  <div v-else-if="loading && !entity" class="mt-6 rounded-lg bg-slate-100 p-4 text-slate-600">Loading entity…</div>
  <div v-else-if="!entity" class="mt-6 rounded-lg bg-slate-100 p-4 text-slate-600">No entity {{ entityId }}.</div>

  <template v-else>
    <header class="mt-4 mb-8">
      <p class="text-sm font-bold uppercase tracking-wider text-slate-500">Entity</p>
      <h1 class="text-4xl font-bold tracking-tight text-slate-900">{{ entity.name }}</h1>
      <p class="mt-1 text-sm text-slate-500">{{ entity.holderUuid }}</p>
    </header>

    <AccountList :accounts="entity.accounts" />
    <InitiateAchForm :entity-id="entity.id" />
    <AchTransactionList :entity-id="entity.id" />
  </template>
</template>
