// Vitest setup file — runs once per test worker before the suite.
//
// We layer @testing-library/jest-dom's custom matchers (toBeInTheDocument,
// toHaveClass, etc.) onto vitest's expect. Without this the imports compile
// but the matchers silently no-op.
import '@testing-library/jest-dom/vitest';

// Per-test cleanup. @testing-library/react autoCleanup runs at afterEach when
// `globals: true` is on (vitest.config.js sets that), so we don't need to
// wire it manually here.
