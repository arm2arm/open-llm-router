import { Link } from 'react-router-dom'
import {
  ArrowUpRight,
  BookOpen,
  Bot,
  Code2,
  GitBranch,
  Layers3,
  Network,
  ShieldCheck,
  Sparkles,
  Terminal,
  Users,
} from 'lucide-react'
import { Button } from '@/components/ui/button'

const repository = 'https://github.com/arm2arm/open-llm-router'

export function AboutPage() {
  return (
    <div className="about-page">
      <a
        className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 bg-background p-3 z-50"
        href="#about-content"
      >
        Skip to content
      </a>
      <header className="about-header">
        <a className="brand" href="/" aria-label="Open LLM Router home">
          <Layers3 className="brand-mark" size={39} />
          <span>Open LLM Router</span>
        </a>
        <Button asChild variant="outline">
          <Link to="/keys">
            Open portal <ArrowUpRight aria-hidden="true" />
          </Link>
        </Button>
      </header>
      <main id="about-content" className="about-content">
        <section className="about-hero" aria-labelledby="about-title">
          <p className="about-eyebrow">Open source · Built at AIP</p>
          <h1 id="about-title">About Open LLM Router</h1>
          <p className="about-lead">
            One gateway for your institution’s language models.
          </p>
          <p>
            Connect locally hosted models, give people access through their
            institution, and share compute through a consistent API. OLR brings
            routing, access controls and usage accounting together in one place.
          </p>
          <div className="about-actions">
            <Button asChild>
              <a href={repository}>
                <Code2 aria-hidden="true" /> Explore the source
              </a>
            </Button>
            <Button asChild variant="outline">
              <a href={`${repository}/tree/main/docs`}>
                <BookOpen aria-hidden="true" /> Documentation
              </a>
            </Button>
          </div>
          <div className="about-badges">
            <span>MIT license</span>
            <span>OpenAI &amp; Anthropic APIs</span>
            <span>Self-hosted infrastructure</span>
          </div>
        </section>

        <section aria-labelledby="about-capabilities">
          <h2 id="about-capabilities">Built for shared model infrastructure</h2>
          <div className="about-feature-grid">
            {[
              {
                icon: Network,
                title: 'Connect your model servers',
                text: 'Use vLLM, SGLang, Ollama and other compatible backends, including services on your internal network.',
              },
              {
                icon: Users,
                title: 'Institutional access',
                text: 'Sign in through Keycloak, issue personal API keys and assign model access to user groups.',
              },
              {
                icon: GitBranch,
                title: 'Route and balance traffic',
                text: 'Give models meaningful names, pool replicas and manage their capacity through the administrator portal.',
              },
              {
                icon: ShieldCheck,
                title: 'Understand usage',
                text: 'Track usage and quotas, with optional personal request traces and user-controlled capture and retention.',
              },
            ].map(({ icon: Icon, title, text }) => (
              <article className="about-feature" key={title}>
                <Icon aria-hidden="true" size={23} />
                <h3>{title}</h3>
                <p>{text}</p>
              </article>
            ))}
          </div>
        </section>

        <section aria-labelledby="about-development">
          <p className="about-eyebrow">People behind the project</p>
          <h2 id="about-development">Coauthors at AIP</h2>
          <div className="about-development-grid">
            <article className="about-person">
              <img
                className="about-avatar"
                src={`${import.meta.env.BASE_URL}people/arman-khalatyan.png`}
                alt="Arman Khalatyan"
                width={96}
                height={96}
                loading="lazy"
              />
              <div>
                <h3>Dr. Arman Khalatyan</h3>
                <p className="about-person-role">
                  Project lead &amp; development
                </p>
                <p>Leibniz Institute for Astrophysics Potsdam (AIP)</p>
                <div className="about-person-links">
                  <a href="https://github.com/arm2arm">
                    GitHub profile <ArrowUpRight aria-hidden="true" size={14} />
                  </a>
                  <a href="https://pages.aip.de/akhalatyan">
                    AIP profile <ArrowUpRight aria-hidden="true" size={14} />
                  </a>
                </div>
              </div>
            </article>
            <article className="about-person">
              <img
                className="about-avatar"
                src={`${import.meta.env.BASE_URL}people/etl-strauss.png`}
                alt="Ing. E.T.L. Strauss"
                width={96}
                height={96}
                loading="lazy"
              />
              <div>
                <h3>Ing. E.T.L. Strauss</h3>
                <p className="about-person-role">Infrastructure Engineer</p>
                <p>Leibniz Institute for Astrophysics Potsdam (AIP)</p>
                <div className="about-person-links">
                  <a href="https://www.aip.de/">
                    AIP institute <ArrowUpRight aria-hidden="true" size={14} />
                  </a>
                </div>
              </div>
            </article>
            <article className="about-contribute">
              <Code2 aria-hidden="true" size={26} />
              <h3>Build with us</h3>
              <p>
                Help improve the router, report a problem or contribute to the
                documentation.
              </p>
              <a href={`${repository}/graphs/contributors`}>
                View contributors <ArrowUpRight aria-hidden="true" size={14} />
              </a>
              <a href={`${repository}/issues`}>
                Issues and ideas <ArrowUpRight aria-hidden="true" size={14} />
              </a>
            </article>
          </div>
          <div className="about-tools">
            <div>
              <h3>AI-assisted development</h3>
              <p>
                Development uses Hermes, OpenAI Codex and Anthropic Claude Code,
                as acknowledged in the repository. Security checks were
                performed using OpenAI’s Daybreaker model.
              </p>
            </div>
            <ul aria-label="Development tools">
              {[
                { icon: Bot, name: 'Hermes' },
                { icon: Terminal, name: 'OpenAI Codex' },
                { icon: Sparkles, name: 'Claude Code' },
              ].map(({ icon: Icon, name }) => (
                <li key={name}>
                  <span className="about-tool-avatar">
                    <Icon aria-hidden="true" size={22} />
                  </span>
                  {name}
                </li>
              ))}
            </ul>
          </div>
        </section>

        <section
          className="about-research-grid"
          aria-label="Funding and research citation"
        >
          <article>
            <p className="about-eyebrow">Acknowledgments</p>
            <h2>Supporting research infrastructure</h2>
            <p>
              Development at AIP was funded by the German Federal Ministry of
              Research, Technology and Space (BMFTR) through the ErUM-Data
              programme as part of Physics-LLM.
            </p>
            <a href="https://physics-llm.erumdatahub.de/">
              Physics-LLM project <ArrowUpRight aria-hidden="true" size={14} />
            </a>
          </article>
          <article>
            <p className="about-eyebrow">For your research</p>
            <h2>Cite the software</h2>
            <blockquote>
              Khalatyan, Arman, &amp; Strauss, E. T. L.{' '}
              <cite>open-llm-router</cite> [Computer software]. Leibniz
              Institute for Astrophysics Potsdam (AIP).
            </blockquote>
            <p>Include the version or commit used in your work.</p>
            <a href={`${repository}/blob/main/CITATION.cff`}>
              Citation metadata <ArrowUpRight aria-hidden="true" size={14} />
            </a>
          </article>
        </section>
      </main>
      <footer className="about-footer">
        <span>
          Open LLM Router · Open-source infrastructure for locally hosted
          models.
        </span>
        <a href={`${repository}/blob/main/LICENSE`}>MIT license</a>
        <a href="https://www.aip.de/">AIP</a>
      </footer>
    </div>
  )
}
