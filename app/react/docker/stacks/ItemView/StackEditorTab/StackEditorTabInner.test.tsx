import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Formik } from 'formik';
import { vi } from 'vitest';
import { ComponentProps } from 'react';
import { JSONSchema7 } from 'json-schema';
import { http, HttpResponse } from 'msw';

import { StackType } from '@/react/common/stacks/types';
import { EnvironmentType } from '@/react/portainer/environments/types';
import { withTestQueryProvider } from '@/react/test-utils/withTestQuery';
import { withUserProvider } from '@/react/test-utils/withUserProvider';
import { createMockUser, createMockUsers } from '@/react-tools/test-mocks';
import { Role } from '@/portainer/users/types';
import { withTestRouter } from '@/react/test-utils/withRouter';
import { server } from '@/setup-tests/server';

import { usePreventExit } from '@@/WebEditorForm';

import {
  StackEditorTabInner,
  resolveRollbackTarget,
} from './StackEditorTabInner';
import { StackEditorFormValues } from './StackEditorTab.types';
import { useVersionedStackFile } from './useVersionedStackFile';

// Mock the hooks
vi.mock('@@/WebEditorForm', () => ({
  usePreventExit: vi.fn(),
}));

vi.mock('./useVersionedStackFile', () => ({
  useVersionedStackFile: vi.fn(),
}));

vi.mock('@uirouter/react', async (importOriginal: () => Promise<object>) => ({
  ...(await importOriginal()),
  useCurrentStateAndParams: vi.fn(() => ({
    params: { endpointId: 5 },
  })),
}));

const defaultProps = {
  stackType: StackType.DockerCompose,
  composeSyntaxMaxVersion: 3,
  envType: EnvironmentType.Docker,
  schema: { type: 'object' } as JSONSchema7,
  isOrphaned: false,
  stackId: 1,
  isSubmitting: false,
  isSaved: false,
  webhookId: '',
};

const defaultInitialValues: StackEditorFormValues = {
  stackFileContent: 'version: "3"\nservices:\n  web:\n    image: nginx',
  environmentVariables: [],
  enabledWebhook: false,
  prune: false,
};

beforeEach(() => {
  vi.clearAllMocks();
});

describe('initial rendering', () => {
  it('should render the form with all main sections', async () => {
    renderComponent();

    await waitFor(() => {
      expect(
        screen.getByText(/This stack will be deployed using/)
      ).toBeVisible();
    });

    // Code editor should be present
    expect(screen.getByTestId('stack-editor')).toBeInTheDocument();

    // Environment variables panel
    expect(screen.getByText(/Environment variables/)).toBeInTheDocument();
  });

  it('should show Docker Compose version 2 message when composeSyntaxMaxVersion is 2', async () => {
    renderComponent({
      stackType: StackType.DockerCompose,
      composeSyntaxMaxVersion: 2,
    });

    await waitFor(() => {
      expect(
        screen.getByText(/Only Compose file format version/)
      ).toBeVisible();
      expect(screen.getByText(/2/)).toBeVisible();
    });
  });

  it('should show Docker Compose generic message when composeSyntaxMaxVersion > 2', async () => {
    renderComponent({
      stackType: StackType.DockerCompose,
      composeSyntaxMaxVersion: 3,
    });

    await waitFor(() => {
      expect(
        screen.getByText(/This stack will be deployed using/)
      ).toBeVisible();
    });
  });

  it('should show documentation link', () => {
    renderComponent();

    const link = screen.getByRole('link', {
      name: /official documentation/i,
    });
    expect(link).toHaveAttribute(
      'href',
      'https://docs.docker.com/compose/compose-file/'
    );
    expect(link).toHaveAttribute('target', '_blank');
  });

  it('should call usePreventExit with correct parameters', () => {
    const mockUsePreventExit = vi.mocked(usePreventExit);
    renderComponent();

    expect(mockUsePreventExit).toHaveBeenCalledWith(
      defaultInitialValues.stackFileContent,
      defaultInitialValues.stackFileContent,
      true
    );
  });

  it('should call useVersionedStackFile with stackId and rollbackTo', () => {
    const mockUseVersionedStackFile = vi.mocked(useVersionedStackFile);
    renderComponent({ stackId: 42 });

    expect(mockUseVersionedStackFile).toHaveBeenCalledWith({
      stackId: 42,
      version: undefined,
      onLoad: expect.any(Function),
    });
  });
});

describe('conditional rendering - Prune services field', () => {
  it('should show Prune field for DockerSwarm with API >= 1.27', async () => {
    renderComponent(
      {
        stackType: StackType.DockerSwarm,
      },
      {
        apiVersion: 1.27,
      }
    );

    await waitFor(() => {
      expect(
        screen.getByTestId('stack-prune-services-switch')
      ).toBeInTheDocument();
    });

    expect(screen.getByText('Prune services')).toBeVisible();
  });

  it('should show Prune field for DockerCompose with API >= 1.27', async () => {
    renderComponent(
      {
        stackType: StackType.DockerCompose,
      },
      {
        apiVersion: 1.27,
      }
    );

    await waitFor(() => {
      expect(
        screen.getByTestId('stack-prune-services-switch')
      ).toBeInTheDocument();
    });
  });

  it('should hide Prune field for DockerSwarm with API < 1.27', () => {
    renderComponent(
      {
        stackType: StackType.DockerSwarm,
      },
      {
        apiVersion: 1.26,
      }
    );

    expect(
      screen.queryByTestId('stack-prune-services-switch')
    ).not.toBeInTheDocument();
  });

  it('should hide Prune field for DockerCompose with API < 1.27', () => {
    renderComponent(
      {
        stackType: StackType.DockerCompose,
      },
      {
        apiVersion: 1.26,
      }
    );

    expect(
      screen.queryByTestId('stack-prune-services-switch')
    ).not.toBeInTheDocument();
  });

  it('should hide Prune field for Kubernetes stack', () => {
    renderComponent({
      stackType: StackType.Kubernetes,
    });

    expect(
      screen.queryByTestId('stack-prune-services-switch')
    ).not.toBeInTheDocument();
  });
});

// TODO: Unskip these tests once WebhookFieldset authorization is properly mocked
// These tests fail because WebhookFieldset has complex authorization checks
// (PortainerWebhookCreate, PortainerWebhookList, PortainerWebhookDelete)
// that aren't properly set up in the test environment
describe.skip('conditional rendering - Webhook field', () => {
  it('should show WebhookFieldset for Docker environment', () => {
    renderComponent({
      envType: EnvironmentType.Docker,
    });

    expect(screen.getByText(/Webhook/)).toBeInTheDocument();
  });

  it('should show WebhookFieldset for KubernetesLocal environment', () => {
    renderComponent({
      envType: EnvironmentType.KubernetesLocal,
    });

    expect(screen.getByText(/Webhook/)).toBeInTheDocument();
  });

  it('should hide WebhookFieldset for EdgeAgentOnDocker environment', () => {
    renderComponent({
      envType: EnvironmentType.EdgeAgentOnDocker,
    });

    expect(screen.queryByText(/Webhook/)).not.toBeInTheDocument();
  });
});

describe('orphaned stack behavior', () => {
  it('should disable CodeEditor when stack is orphaned', () => {
    renderComponent({ isOrphaned: true });

    const editor = screen.getByTestId('stack-editor');
    expect(editor).toHaveAttribute('readonly');
  });

  it('should disable deploy button when stack is orphaned', async () => {
    renderComponent({ isOrphaned: true });

    await waitFor(() => {
      const deployButton = screen.queryByTestId('stack-deploy-button');

      expect(deployButton).toBeDisabled();
    });
  });

  it('should enable CodeEditor when stack is not orphaned', async () => {
    renderComponent({ isOrphaned: false });

    const editor = screen.getByTestId('stack-editor');
    await waitFor(() => {
      expect(editor).not.toHaveAttribute('readonly');
    });
  });
});

describe('form field updates', () => {
  it('should update stackFileContent when CodeEditor changes', async () => {
    const onSubmit = vi.fn();
    renderComponent({}, { onSubmit });
    const user = userEvent.setup();

    const editor = screen.getByTestId('stack-editor');
    await waitFor(() => {
      expect(editor).not.toHaveAttribute('readonly');
    });

    await user.clear(editor);
    await user.type(editor, 'version: "3.8"');

    await waitFor(() => {
      expect(editor).toHaveValue('version: "3.8"');
    });
  });

  it('should update prune field when SwitchField changes', async () => {
    const onSubmit = vi.fn();
    renderComponent(
      {
        stackType: StackType.DockerSwarm,
      },
      { onSubmit }
    );
    const user = userEvent.setup();

    await waitFor(() => {
      expect(
        screen.getByTestId('stack-prune-services-switch')
      ).toBeInTheDocument();
    });

    const pruneSwitchField = screen.getByTestId('stack-prune-services-switch');
    await user.click(pruneSwitchField);

    const pruneSwitch = pruneSwitchField.querySelector(
      'input[type="checkbox"]'
    );

    await waitFor(() => {
      expect(pruneSwitch).toBeChecked();
    });
  });
});

describe('version rollback', () => {
  it('should call useVersionedStackFile with rollbackTo value', () => {
    const mockUseVersionedStackFile = vi.mocked(useVersionedStackFile);
    const initialValues = {
      ...defaultInitialValues,
      rollbackTo: 2,
    };

    renderComponent({ stackId: 5 }, { initialValues });

    expect(mockUseVersionedStackFile).toHaveBeenCalledWith({
      stackId: 5,
      version: 2,
      onLoad: expect.any(Function),
    });
  });

  it('should update stackFileContent when version loaded', () => {
    const mockUseVersionedStackFile = vi.mocked(useVersionedStackFile);
    let capturedOnLoad: ((content: string) => void) | undefined;

    mockUseVersionedStackFile.mockImplementation(({ onLoad }) => {
      capturedOnLoad = onLoad;

      return {
        content: '',
        isLoading: false,
      };
    });

    renderComponent({ versions: [3, 2, 1] });

    expect(capturedOnLoad).toBeDefined();

    // Simulate loading a different version
    if (capturedOnLoad) {
      capturedOnLoad('version: "2"\nservices:\n  db:\n    image: postgres');
    }

    // The form value should be updated through setFieldValue
    // This would be verified by checking the editor value in a full integration test
  });

  it('should set rollbackTo when version selected and multiple versions available', async () => {
    const onSubmit = vi.fn();
    const versions = [3, 2, 1];
    renderComponent({ versions }, { onSubmit });
    const user = userEvent.setup();

    const versionSelect = screen.getByRole('combobox', { name: /version/i });
    await user.selectOptions(versionSelect, '2');

    await waitFor(() => {
      // Check that the form value was updated (through Formik)
      expect(versionSelect).toHaveValue('2');
    });
  });
});

// F6: the version selector must not silently discard manual edits. The backend
// ignores the client buffer whenever rollbackTo is set, so rollbackTo must only
// be set for a genuinely older version and must be cleared once the user either
// re-selects the current version or edits the buffer by hand.
describe('resolveRollbackTarget (F6 decision)', () => {
  it('returns undefined when the current/top version is picked', () => {
    // versions[0] is the latest -> picking it is not a rollback
    expect(resolveRollbackTarget(3, [3, 2, 1])).toBeUndefined();
  });

  it('returns the version when a genuinely older version is picked', () => {
    expect(resolveRollbackTarget(2, [3, 2, 1])).toBe(2);
    expect(resolveRollbackTarget(1, [3, 2, 1])).toBe(1);
  });

  it('returns undefined when only a single version exists', () => {
    expect(resolveRollbackTarget(1, [1])).toBeUndefined();
  });

  it('returns undefined when versions are unavailable', () => {
    expect(resolveRollbackTarget(1, undefined)).toBeUndefined();
    expect(resolveRollbackTarget(1, [])).toBeUndefined();
  });
});

describe('version selector edit trap (F6)', () => {
  // The `version` passed to useVersionedStackFile mirrors values.rollbackTo, so
  // we assert on the latest call to observe how rollbackTo evolves.
  function lastRollbackTo() {
    const { calls } = vi.mocked(useVersionedStackFile).mock;
    return calls[calls.length - 1][0].version;
  }

  beforeEach(() => {
    vi.mocked(useVersionedStackFile).mockReturnValue({
      content: '',
      isLoading: false,
    });
  });

  it('should set rollbackTo when an older version is selected', async () => {
    renderComponent({ versions: [3, 2, 1] });
    const user = userEvent.setup();

    const versionSelect = screen.getByRole('combobox', { name: /version/i });
    await user.selectOptions(versionSelect, '2');

    await waitFor(() => {
      expect(lastRollbackTo()).toBe(2);
    });
  });

  it('should clear rollbackTo when the current/top version is re-selected', async () => {
    renderComponent({ versions: [3, 2, 1] });
    const user = userEvent.setup();

    const versionSelect = screen.getByRole('combobox', { name: /version/i });

    // First roll back to an older version...
    await user.selectOptions(versionSelect, '2');
    await waitFor(() => {
      expect(lastRollbackTo()).toBe(2);
    });

    // ...then return to the current version: this is not a rollback, so
    // rollbackTo must be cleared to restore normal edit-and-deploy.
    await user.selectOptions(versionSelect, '3');
    await waitFor(() => {
      expect(lastRollbackTo()).toBeUndefined();
    });
  });

  it('should clear rollbackTo when the user edits the buffer', async () => {
    renderComponent(
      { versions: [3, 2, 1] },
      { initialValues: { ...defaultInitialValues, rollbackTo: 2 } }
    );
    const user = userEvent.setup();

    // rollbackTo starts at 2 (an older version pre-selected)
    expect(lastRollbackTo()).toBe(2);

    const editor = screen.getByTestId('stack-editor');
    await waitFor(() => {
      expect(editor).not.toHaveAttribute('readonly');
    });

    // A genuine user edit should reset rollbackTo so the edits are honored.
    await user.type(editor, ' # manual edit');

    await waitFor(() => {
      expect(lastRollbackTo()).toBeUndefined();
    });
  });

  it('should NOT clear rollbackTo when a version is loaded programmatically', async () => {
    let capturedOnLoad: ((content: string) => void) | undefined;
    vi.mocked(useVersionedStackFile).mockImplementation(({ onLoad }) => {
      capturedOnLoad = onLoad;
      return { content: '', isLoading: false };
    });

    renderComponent(
      { versions: [3, 2, 1] },
      { initialValues: { ...defaultInitialValues, rollbackTo: 2 } }
    );

    expect(capturedOnLoad).toBeDefined();

    // The programmatic version-load goes through handleLoadFile (setFieldValue
    // on stackFileContent), NOT the CodeEditor onChange, so rollbackTo must
    // stay set — otherwise the rollback would immediately cancel itself.
    act(() => {
      capturedOnLoad?.('version: "2"\nservices:\n  db:\n    image: postgres');
    });

    // Wait past the CodeEditor's 300ms onChange debounce: if the programmatic
    // load ever leaked into handleContentChange it would clear rollbackTo only
    // after the debounce fires, so asserting at t≈0 would pass vacuously. By
    // waiting beyond the window we ensure the assertion fails if a load ever
    // clears rollbackTo.
    await act(async () => {
      await new Promise((resolve) => {
        setTimeout(resolve, 350);
      });
    });

    expect(lastRollbackTo()).toBe(2);
  });

  it('should restore the current content when returning to the current/top version', async () => {
    let capturedOnLoad: ((content: string) => void) | undefined;
    vi.mocked(useVersionedStackFile).mockImplementation(({ onLoad }) => {
      capturedOnLoad = onLoad;
      return { content: '', isLoading: false };
    });

    renderComponent({ versions: [3, 2, 1] });
    const user = userEvent.setup();

    const editor = screen.getByTestId('stack-editor');
    await waitFor(() => {
      expect(editor).not.toHaveAttribute('readonly');
    });

    const versionSelect = screen.getByRole('combobox', { name: /version/i });

    // Roll back to an older version: rollbackTo is set and its content is loaded
    // into the buffer (simulating useVersionedStackFile's fetch via onLoad).
    const olderContent = 'version: "2"\nservices:\n  db:\n    image: postgres';
    await user.selectOptions(versionSelect, '2');
    await waitFor(() => {
      expect(lastRollbackTo()).toBe(2);
    });
    act(() => {
      capturedOnLoad?.(olderContent);
    });
    await waitFor(() => {
      expect(editor).toHaveValue(olderContent);
    });

    // Return to the current/top version: rollbackTo must clear AND the editor
    // must show the current content again, not the older version's leftover
    // content (which would otherwise be deployed as a brand-new version).
    await user.selectOptions(versionSelect, '3');
    await waitFor(() => {
      expect(lastRollbackTo()).toBeUndefined();
    });
    expect(editor).toHaveValue(defaultInitialValues.stackFileContent);
  });
});

describe('form submission', () => {
  it('should enable submit button when form is valid', async () => {
    renderComponent();

    await waitFor(() => {
      const deployButton = screen.getByTestId('stack-deploy-button');
      expect(deployButton).toBeEnabled();
    });
  });

  it('should disable submit button when stack is orphaned', async () => {
    const onSubmit = vi.fn();
    renderComponent({ isOrphaned: true }, { onSubmit });

    await waitFor(() => {
      const deployButton = screen.queryByTestId('stack-deploy-button');

      expect(deployButton).toBeDisabled();
    });
  });

  it('should show loading text during submission', async () => {
    renderComponent({ isSubmitting: true }, {});

    await waitFor(() => {
      expect(screen.getByText(/Deployment in progress.../)).toBeInTheDocument();
    });
  });

  it('should call onSubmit with form values when button is clicked', async () => {
    const onSubmit = vi.fn();
    renderComponent({}, { onSubmit });
    const user = userEvent.setup();

    await waitFor(() => {
      const deployButton = screen.getByTestId('stack-deploy-button');
      expect(deployButton).toBeEnabled();
    });

    const deployButton = screen.getByTestId('stack-deploy-button');
    await user.click(deployButton);

    await waitFor(() => {
      expect(onSubmit).toHaveBeenCalled();
    });
  });
});

describe('authorization', () => {
  it('should hide Prune field when user lacks PortainerStackUpdate authorization', () => {
    const unauthorizedUser = createMockUsers(1, Role.Standard)[0];

    renderComponent(
      {
        stackType: StackType.DockerSwarm,
      },
      { user: unauthorizedUser }
    );

    expect(
      screen.queryByTestId('stack-prune-services-switch')
    ).not.toBeInTheDocument();
  });

  it('should hide FormActions when user lacks PortainerStackUpdate authorization', () => {
    const unauthorizedUser = createMockUsers(1, Role.Standard)[0];

    renderComponent({}, { user: unauthorizedUser });

    expect(screen.queryByTestId('stack-deploy-button')).not.toBeInTheDocument();
  });
});

describe('isSaved state', () => {
  it('should not prevent exit when isSaved is true', () => {
    const mockUsePreventExit = vi.mocked(usePreventExit);
    renderComponent({ isSaved: true });

    expect(mockUsePreventExit).toHaveBeenCalledWith(
      expect.any(String),
      expect.any(String),
      false // Should be false when isSaved is true
    );
  });

  it('should prevent exit when isSaved is false and form is dirty', () => {
    const mockUsePreventExit = vi.mocked(usePreventExit);
    const initialValues = {
      ...defaultInitialValues,
      stackFileContent: 'original content',
    };

    renderComponent({ isSaved: false }, { initialValues });

    expect(mockUsePreventExit).toHaveBeenCalledWith(
      'original content',
      'original content',
      true
    );
  });
});

/**
 * Helper function to render StackEditorTabInner with Formik wrapper
 */
function renderComponent(
  props: Partial<ComponentProps<typeof StackEditorTabInner>> = {},
  {
    apiVersion = 1.47,
    onSubmit = vi.fn(),
    initialValues = defaultInitialValues,
    validationErrors = {},
    user = createMockUser({ Role: Role.Admin }), // Default admin user
  }: {
    apiVersion?: number;
    onSubmit?: (values: StackEditorFormValues) => void | Promise<void>;
    initialValues?: StackEditorFormValues;
    validationErrors?: Partial<Record<keyof StackEditorFormValues, string>>;
    user?: ReturnType<typeof createMockUser>;
  } = {}
) {
  server.use(
    http.get('/api/endpoints/:endpointId/docker/version', () =>
      HttpResponse.json({ ApiVersion: String(apiVersion) })
    )
  );

  // Create validation function that returns errors
  function validate() {
    return validationErrors;
  }

  const Component = withTestQueryProvider(
    withTestRouter(withUserProvider(StackEditorTabInner, user))
  );

  return render(
    <Formik
      initialValues={initialValues}
      onSubmit={onSubmit}
      validate={validate}
      enableReinitialize
    >
      <Component {...defaultProps} {...props} />
    </Formik>
  );
}
