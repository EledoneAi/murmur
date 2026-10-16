// The hub's web pages. `pnpm build` writes static files into the hub's package, which embeds them in its binary;
// `pnpm dev` serves the pages with hot reload and hands everything else to a hub running at HUB (make dev).
import { defineConfig } from 'astro/config'
import svelte from '@astrojs/svelte'
import tailwindcss from '@tailwindcss/vite'

const hub = process.env.HUB || 'http://localhost:8080'
const demo = process.env.DEMO === '1' // pnpm demo: a made-up team, no hub

// demoHub answers what the pages ask a hub, from the made-up team in demo/, and streams it every two seconds.
function demoHub() {
  return {
    name: 'murmur-demo-hub',
    async configureServer(server) {
      const data = await import('./demo/team.mjs')
      const json = (res, v, status = 200) => { res.statusCode = status; res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify(v)) }
      server.middlewares.use((req, res, next) => {
        const url = req.url.split('?')[0]
        if (url === '/api/me') return json(res, data.me())
        if (url === '/api/home') return json(res, data.home())
        if (url === '/api/team') return json(res, data.team())
        const admin = { '/api/admin': data.adminOverview, '/api/admin/people': data.adminPeople, '/api/admin/laptops': data.adminLaptops,
          '/api/admin/settings': data.adminSettings, '/api/admin/events': data.adminEvents }
        if (req.method === 'GET' && admin[url]) return json(res, admin[url]())
        if (req.method === 'POST' && url === '/api/admin/settings') return json(res, { ok: true, ...data.adminSettings() })
        if (req.method === 'POST' && url.startsWith('/api/admin/')) return json(res, { ok: true, join: data.adminPeople().join, you: false, admin: true })
        if (url === '/team/send' || url === '/api/devices/revoke') return json(res, { ok: true, result: 'Delivered.' })
        if (url === '/team/events') {
          res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' })
          let step = 0
          const send = () => res.write(`data: ${JSON.stringify(data.team(step++))}\n\n`)
          send()
          const t = setInterval(send, 2000)
          req.on('close', () => clearInterval(t))
          return
        }
        next()
      })
    },
  }
}

// The paths the hub answers itself: its API, sign-in, the live stream, laptops' endpoints.
const hubPaths = ['/api', '/auth', '/login', '/dev-login', '/logout', '/cli', '/team/events', '/team/send', '/team/prs',
  '/install.sh', '/dl', '/ws', '/github']

export default defineConfig({
  outDir: '../internal/hub/webdist',
  integrations: [svelte()],
  build: { format: 'directory', inlineStylesheets: 'never' },
  devToolbar: { enabled: false },
  vite: {
    plugins: [tailwindcss(), ...(demo ? [demoHub()] : [])],
    // every script is a file the hub serves: its Content-Security-Policy allows no inline script
    build: { assetsInlineLimit: 0 },
    server: demo ? {} : {
      proxy: Object.fromEntries(hubPaths.map(p => [p, {
        target: hub, changeOrigin: true, ws: p === '/ws',
        // the hub refuses posts from another origin: in dev, the pages' origin is this server's
        configure: proxy => proxy.on('proxyReq', req => { if (req.getHeader('origin')) req.setHeader('origin', hub) }),
      }])),
    },
  },
})
