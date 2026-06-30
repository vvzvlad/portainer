import { PageHeader } from '@@/PageHeader';
import { Widget, WidgetBody } from '@@/Widget';

export function ActivityLogsView() {
  return (
    <>
      <PageHeader
        title="User activity logs"
        breadcrumbs="User activity logs"
        reload
      />

      <div className="mx-4">
        <Widget>
          <WidgetBody>
            <div className="text-center text-muted">
              No activity logs available.
            </div>
          </WidgetBody>
        </Widget>
      </div>
    </>
  );
}
