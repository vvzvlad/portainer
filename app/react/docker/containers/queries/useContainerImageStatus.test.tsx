import { renderHook } from '@testing-library/react-hooks';
import { http, HttpResponse } from 'msw';

import { server } from '@/setup-tests/server';
import { withTestQueryProvider } from '@/react/test-utils/withTestQuery';

import {
  ContainerImageStatus,
  useContainerImageStatus,
  useRecheckContainerImageStatus,
} from './useContainerImageStatus';

const environmentId = 3;
const containerId = 'abc123';

function renderImageStatusHook(nodeName?: string) {
  return renderHook(
    () => useContainerImageStatus(environmentId, containerId, nodeName),
    { wrapper: withTestQueryProvider(({ children }) => <>{children}</>) }
  );
}

describe('useContainerImageStatus', () => {
  it('calls the non-proxied image_status endpoint and returns the status', async () => {
    const payload: ContainerImageStatus = { Status: 'outdated' };

    server.use(
      http.get(
        `/api/docker/${environmentId}/containers/${containerId}/image_status`,
        () => HttpResponse.json(payload)
      )
    );

    const { result, waitFor } = renderImageStatusHook();

    await waitFor(() => result.current.isSuccess);
    expect(result.current.data).toEqual(payload);
  });

  it('forwards nodeName as a query parameter', async () => {
    let receivedNodeName: string | null = null;

    server.use(
      http.get(
        `/api/docker/${environmentId}/containers/${containerId}/image_status`,
        ({ request }) => {
          receivedNodeName = new URL(request.url).searchParams.get('nodeName');
          return HttpResponse.json<ContainerImageStatus>({ Status: 'updated' });
        }
      )
    );

    const { result, waitFor } = renderImageStatusHook('node-2');

    await waitFor(() => result.current.isSuccess);
    expect(receivedNodeName).toBe('node-2');
  });

  it('does not send the force parameter on the background query', async () => {
    let receivedForce: string | null = 'unset';

    server.use(
      http.get(
        `/api/docker/${environmentId}/containers/${containerId}/image_status`,
        ({ request }) => {
          receivedForce = new URL(request.url).searchParams.get('force');
          return HttpResponse.json<ContainerImageStatus>({ Status: 'updated' });
        }
      )
    );

    const { result, waitFor } = renderImageStatusHook();

    await waitFor(() => result.current.isSuccess);
    expect(receivedForce).toBeNull();
  });
});

describe('useRecheckContainerImageStatus', () => {
  it('sends force=true and updates the shared query cache with the fresh status', async () => {
    let receivedForce: string | null = null;

    server.use(
      http.get(
        `/api/docker/${environmentId}/containers/${containerId}/image_status`,
        ({ request }) => {
          receivedForce = new URL(request.url).searchParams.get('force');
          return HttpResponse.json<ContainerImageStatus>({
            Status: 'outdated',
          });
        }
      )
    );

    const wrapper = withTestQueryProvider(({ children }) => <>{children}</>);
    const { result, waitFor } = renderHook(
      () => useRecheckContainerImageStatus(environmentId, containerId),
      { wrapper }
    );

    result.current.mutate();

    await waitFor(() => result.current.isSuccess);
    expect(receivedForce).toBe('true');
    expect(result.current.data).toEqual({ Status: 'outdated' });
  });
});
