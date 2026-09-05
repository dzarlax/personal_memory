import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

const configuredBase = process.env.DOCS_BASE?.trim() || '/personal_memory';
const base = configuredBase === '/'
  ? '/'
  : `/${configuredBase.replace(/^\/+|\/+$/g, '')}`;
const site = process.env.DOCS_SITE || undefined;

export default defineConfig({
  ...(site ? { site } : {}),
  base,
  integrations: [
    starlight({
      title: 'Personal Memory',
      sidebar: [
        { label: 'Start here', items: [{ slug: 'index' }, { slug: 'whats-new' }, { slug: 'getting-started/installation' }, { slug: 'getting-started/connect-clients' }, { slug: 'architecture-security' }, { slug: 'limitations' }] },
        { label: 'Operations', items: [{ slug: 'operations/upgrade-rollback' }, { slug: 'operations/backups-release' }, { slug: 'operations/release-report-v0-1-0' }, { slug: 'operations/troubleshooting' }, { slug: 'operations/evaluation' }, { slug: 'operations/conformance' }] },
        { label: 'Lifecycle and maintenance', items: [{ slug: 'lifecycle/fact-lifecycle-contract' }, { slug: 'lifecycle/migration' }, { slug: 'maintenance' }] },
        { label: 'Reference', items: [{ slug: 'reference/tools' }, { slug: 'reference/configuration' }, { slug: 'reference/compatibility' }, { slug: 'reference/model-memory-usage-contract' }] },
        { label: 'Integration bundle', items: [{ slug: 'integration-bundle/guide' }] },
      ],
      social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/Dzarlax-AI/personal_memory' }],
      editLink: { baseUrl: 'https://github.com/Dzarlax-AI/personal_memory/edit/main/website/' },
    }),
  ],
});
