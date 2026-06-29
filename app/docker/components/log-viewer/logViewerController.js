import moment from 'moment';

import { concatLogsToString, NEW_LINE_BREAKER } from '@/docker/helpers/logHelper';

angular.module('portainer.docker').controller('LogViewerController', [
  '$scope',
  'clipboard',
  'Blob',
  'FileSaver',
  function ($scope, clipboard, Blob, FileSaver) {
    this.state = {
      availableSinceDatetime: [
        { desc: 'Last day', value: moment().subtract(1, 'days').format() },
        { desc: 'Last 4 hours', value: moment().subtract(4, 'hours').format() },
        { desc: 'Last hour', value: moment().subtract(1, 'hours').format() },
        { desc: 'Last 10 minutes', value: moment().subtract(10, 'minutes').format() },
      ],
      copySupported: clipboard.supported,
      logCollection: true,
      autoScroll: true,
      wrapLines: true,
      search: '',
      filteredLogs: [],
      selectedLines: [],
    };

    this.handleLogsCollectionChange = handleLogsCollectionChange.bind(this);
    this.handleLogsWrapLinesChange = handleLogsWrapLinesChange.bind(this);
    this.handleDisplayTimestampsChange = handleDisplayTimestampsChange.bind(this);
    this.applyFilter = applyFilter.bind(this);

    this.$onInit = function () {
      this.applyFilter();
      // Compute the filtered list in the controller (not in the template) so we
      // do not rebuild `filteredLogs` on every digest. `$watchCollection` only
      // fires when lines are actually appended; combined with `track by log.id`
      // in the template, already-rendered rows are never re-bound — so a live
      // text selection survives incoming log lines.
      $scope.$watchCollection(() => this.data, this.applyFilter);
      $scope.$watch(() => this.state.search, this.applyFilter);
    };

    function applyFilter() {
      const data = this.data || [];
      const search = (this.state.search || '').toLowerCase();
      this.state.filteredLogs = search
        ? data.filter((log) => log.line && log.line.toLowerCase().indexOf(search) > -1)
        : data;
    }

    function handleLogsCollectionChange(enabled) {
      $scope.$evalAsync(() => {
        // Decouple Live (log collection) from auto-scroll: pausing the stream no
        // longer also forces auto-scroll off, and auto-scroll is driven by
        // scroll-glue (it disengages when the user scrolls up, re-engages at the
        // bottom) so reading/selecting is never yanked around.
        this.state.logCollection = enabled;
        this.logCollectionChange(enabled);
      });
    }

    function handleLogsWrapLinesChange(enabled) {
      $scope.$evalAsync(() => {
        this.state.wrapLines = enabled;
      });
    }

    function handleDisplayTimestampsChange(enabled) {
      $scope.$evalAsync(() => {
        this.displayTimestamps = enabled;
      });
    }

    this.copy = function () {
      clipboard.copyText(this.state.filteredLogs.map((log) => log.line).join(NEW_LINE_BREAKER));
      $('#refreshRateChange').show();
      $('#refreshRateChange').fadeOut(2000);
    };

    this.copySelection = function () {
      clipboard.copyText(this.state.selectedLines.join(NEW_LINE_BREAKER));
      $('#refreshRateChange').show();
      $('#refreshRateChange').fadeOut(2000);
    };

    this.clearSelection = function () {
      this.state.selectedLines = [];
    };

    this.selectLine = function (line) {
      var idx = this.state.selectedLines.indexOf(line);
      if (idx === -1) {
        this.state.selectedLines.push(line);
      } else {
        this.state.selectedLines.splice(idx, 1);
      }
    };

    this.downloadLogs = function () {
      const logsAsString = concatLogsToString(this.state.filteredLogs);
      const data = new Blob([logsAsString]);
      FileSaver.saveAs(data, this.resourceName + '_logs.txt');
    };
  },
]);
