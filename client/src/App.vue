<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'

const route = useRoute()
const wide = computed(() => route.meta.wide === true)
</script>

<template>
  <main class="flex min-h-screen justify-center bg-slate-50 px-6 py-12">
    <div class="w-full" :class="wide ? 'max-w-7xl' : 'max-w-3xl'">
      <nav class="mb-6 flex gap-4 text-sm font-bold text-blue-700">
        <RouterLink :to="{ name: 'entities' }" class="hover:underline">Entities</RouterLink>
        <RouterLink :to="{ name: 'money-flow' }" class="hover:underline">Money flow</RouterLink>
      </nav>
      <RouterView />
    </div>
  </main>
</template>
