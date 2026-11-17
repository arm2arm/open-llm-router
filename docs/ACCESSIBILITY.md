# Portal accessibility checks

The required portal browser job runs `web/tests/accessibility.spec.ts` with
`@axe-core/playwright`. It checks the public home page and the key, usage, client,
request and administration views against axe's WCAG A/AA rules, including WCAG
2.1 and 2.2, in both portal themes. **There are no axe rule or selector exclusions.**
Checks use synthetic API responses and real page components. Buttons change their disabled opacity immediately, avoiding a low-contrast fade
when loading completes.

The same suite checks input, focus, switch and text contrast from computed colors;
a single chart tab stop and arrow/Home/End navigation including zero-value days;
logging-save focus and status; route titles; named JSON disclosure controls;
request filters outside live regions; short-screen dialogs; and 320/390-pixel agent
access reflow. Horizontally scrolling code examples are named, focusable regions.

`npm run lint` enables the recommended `eslint-plugin-jsx-a11y` rules and rejects
named `div` containers without a role. The tabindex rule permits named `region`
containers so keyboard users can scroll code examples. ESLint and `@eslint/js`
are pinned to the compatible 9.39.5 release while jsx-a11y 6.10.2 declares support
through ESLint 9. Its minimatch dependency uses the patched 3.1.5 API; other
minimatch consumers retain the project override. `npm audit` checks the lockfile.

Run locally after starting the app:

```sh
cd web
npm ci
npm run lint
npx playwright test tests/accessibility.spec.ts
```

`OLR_BASE_URL` selects the router; `OLR_HOME_URL` optionally selects its public home
page when the portal is served by Vite. Browser traces, video and screenshots are
disabled by default because other browser journeys create credentials.

Automated checks cover only detectable issues. These checks do not establish
WCAG conformance; keyboard and assistive-technology review remain useful.
See [axe Playwright](https://github.com/dequelabs/axe-core-npm/tree/develop/packages/playwright)
and [jsx-a11y](https://github.com/jsx-eslint/eslint-plugin-jsx-a11y).
